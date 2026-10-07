<script setup lang="ts">
import { computed, onMounted, ref } from "vue";
import { useRouter } from "vue-router";
import { adminApi, fetchCipherRange, streamUrlWithToken } from "@/api/endpoints";
import type { DeliveryMode, StorageProbeSession, StorageStatus } from "@/api/types";
import { encryptionSupported } from "@/crypto/clientkey";
import { base64ToBytes, decryptBlock, importContentKey, parseXphHeader } from "@/crypto/xph";
import { Sha256, bytesToHex } from "@/crypto/sha256";
import AdminPage from "@/components/admin/AdminPage.vue";
import StatStrip from "@/components/admin/StatStrip.vue";
import Panel from "@/components/admin/Panel.vue";
import AppButton from "@/components/ui/AppButton.vue";
import { topbarSlot } from "@/stores/shell";
import { describeError, logError } from "@/lib/async";
import { formatTime } from "@/lib/format";

// 存储后端状态与连通性自检。
//
// 自检由前端主导：后端只上传一个探针文件并按当前配置准备交付通道，
// 前端自己向直链 URL / 中转 URL 发起请求，下载、解密、比对摘要，
// 从而实测出「当前下载通道」究竟走哪一条。

const router = useRouter();

const status = ref<StorageStatus>({
  kind: "",
  ready: false,
  detail: "",
  credentialsConfigured: false,
  presignReady: false,
  authCallback: false,
  directLinkConfigured: false,
  proxyDecrypt: false,
});
const loading = ref(false);
const error = ref("");

const probing = ref(false);
const probeSession = ref<StorageProbeSession | null>(null);
const probeStartError = ref("");
const verifyResult = ref<ProbeVerify | null>(null);
const cleaned = ref<boolean | null>(null);
const finishError = ref("");

interface ProbeVerify {
  ok: boolean;
  downloaded: boolean;
  verified: boolean;
  bytesGot: number;
  latencyMs: number;
  detail: string;
}

async function load(): Promise<void> {
  loading.value = true;
  error.value = "";
  try {
    status.value = await adminApi.storageStatus();
  } catch (err) {
    error.value = describeError(err);
    logError("admin.storage", err);
  } finally {
    loading.value = false;
  }
}

onMounted(load);

/**
 * 执行一次完整自检：start（后端上传探针、准备通道）→ 前端实测下载与解密 →
 * finish（无论成败都清理远端对象与临时票据）。
 */
async function probe(): Promise<void> {
  probing.value = true;
  probeSession.value = null;
  probeStartError.value = "";
  verifyResult.value = null;
  cleaned.value = null;
  finishError.value = "";

  let session: StorageProbeSession | null = null;
  try {
    try {
      session = await adminApi.storageProbeStart();
      probeSession.value = session;
    } catch (err) {
      // 探针上传/通道准备失败：没有会话，也就没有后续实测与清理。
      probeStartError.value = describeError(err);
      logError("admin.storage.probe", err);
      return;
    }

    try {
      verifyResult.value = await verifyProbeSession(session);
    } catch (err) {
      verifyResult.value = {
        ok: false,
        downloaded: false,
        verified: false,
        bytesGot: 0,
        latencyMs: 0,
        detail: describeError(err),
      };
    }
  } finally {
    if (session) {
      try {
        await adminApi.storageProbeFinish(
          session.fileId,
          {
            mode: session.mode,
            ok: verifyResult.value?.ok ?? false,
            detail: verifyResult.value?.detail ?? "",
            bytesGot: verifyResult.value?.bytesGot ?? 0,
            latencyMs: verifyResult.value?.latencyMs ?? 0,
          },
        );
        cleaned.value = true;
      } catch (err) {
        cleaned.value = false;
        finishError.value = describeError(err);
        logError("admin.storage.probe.finish", err);
      }
    }
    probing.value = false;
    void load();
  }
}

/**
 * 按探针会话声明的模式发起一次真实下载并验证内容。
 *
 * proxy_decrypt：响应体即明文，直接比对长度与 SHA-256；
 * direct/proxy ：拿到的是密文（直链跨域或本机纯代理中转，处理完全一致），
 *                本地解析 XPH 文件头、用随会话下发的 DEK 解密后比对摘要。
 */
async function verifyProbeSession(session: StorageProbeSession): Promise<ProbeVerify> {
  const started = performance.now();
  if (session.mode === "proxy_decrypt") {
    if (!session.streamUrl) {
      throw new Error("服务端未提供中转地址");
    }
    const response = await fetch(streamUrlWithToken(session.streamUrl), { cache: "no-store" });
    if (!response.ok) {
      throw new Error(`中转下载失败：HTTP ${response.status}`);
    }
    const bytes = new Uint8Array(await response.arrayBuffer());
    if (bytes.byteLength !== session.sizePlain) {
      throw new Error(`明文长度不符：期望 ${session.sizePlain}，实际 ${bytes.byteLength}`);
    }
    if (digestHex(bytes) !== session.plainSha256) {
      throw new Error("明文摘要不一致：中转解密内容可能被破坏");
    }
    return {
      ok: true,
      downloaded: true,
      verified: true,
      bytesGot: bytes.byteLength,
      latencyMs: Math.round(performance.now() - started),
      detail: "",
    };
  }

  // 直链加密 / 中转加密：必须在浏览器本地解密。非安全上下文没有 WebCrypto，
  // 此时真实下载也会被后端自动落到 proxy_decrypt，因此这里明确提示而不是
  // 给出"校验失败"的误导结论。
  if (!encryptionSupported()) {
    throw new Error("当前浏览器上下文不支持本地解密（需要安全上下文 HTTPS 与 WebCrypto）");
  }
  if (!session.key || session.blockLog2 === undefined || session.noncePrefix === undefined) {
    throw new Error("服务端未下发解密所需的密钥与文件头材料");
  }
  const url =
    session.mode === "direct" ? session.directUrl : streamUrlWithToken(session.streamUrl ?? "");
  if (!url) {
    throw new Error("服务端未提供密文下载地址");
  }

  // 探针只有 1 字节明文，密文仅几十字节，整份读回即可——这也是对完整对象
  // （文件头 + 整块密文 + tag）的一次真实校验。
  let cipher: Uint8Array;
  try {
    cipher = await fetchCipherRange(url, 0, session.sizeWire - 1);
  } catch (err) {
    throw withAuthCallbackHint(err, session.mode);
  }
  const header = parseXphHeader(cipher.subarray(0, 64));
  const headerProblems: string[] = [];
  if (header.blockLog2 !== session.blockLog2) {
    headerProblems.push(`块大小指数 头=${header.blockLog2} 会话=${session.blockLog2}`);
  }
  if (header.noncePrefix !== session.noncePrefix) {
    headerProblems.push(`nonce 前缀 头=${header.noncePrefix} 会话=${session.noncePrefix}`);
  }
  if (header.plainSize !== session.sizePlain) {
    headerProblems.push(`明文长度 头=${header.plainSize} 会话=${session.sizePlain}`);
  }
  if (header.cipherSize !== session.sizeWire) {
    headerProblems.push(`密文长度 头=${header.cipherSize} 会话=${session.sizeWire}`);
  }
  if (headerProblems.length > 0) {
    throw new Error(`对象与记录不一致（${headerProblems.join("；")}）`);
  }

  const key = await importContentKey(base64ToBytes(session.key));
  const plain = await decryptBlock(key, header, 0, cipher.subarray(64));
  if (digestHex(plain) !== session.plainSha256) {
    throw new Error("解密后的内容摘要不一致");
  }
  return {
    ok: true,
    downloaded: true,
    verified: true,
    bytesGot: plain.byteLength,
    latencyMs: Math.round(performance.now() - started),
    detail: "",
  };
}

function digestHex(bytes: Uint8Array): string {
  return bytesToHex(new Sha256().update(bytes).digest());
}

/**
 * 给直链 403 补上配置指引。
 *
 * 直链的字节由 123 出，123 受理每个请求时会回源问我们"这条能不能放"，我们答
 * 403 时它就把拒绝原样退给浏览器——所以管理端只看得见一个裸 403，具体是票据
 * 没回传、票据失效还是访客地址没带上，只有服务端日志分得清。这里直接把 123
 * 面板该怎么填写摆出来，省掉一轮盲猜。
 *
 * 只在直链 + 已开启回源鉴权 + 403 时追加：其余情况原样抛出，避免把一条准确的
 * 报错稀释成猜测。
 */
function withAuthCallbackHint(err: unknown, mode: string): Error {
  const base = err instanceof Error ? err : new Error(String(err));
  if (mode !== "direct" || !status.value.authCallback || !base.message.includes("HTTP 403")) {
    return base;
  }
  return new Error(
    `${base.message}。请按「系统配置 → 123 云盘 → CDN 回源鉴权」把参数配置妥当：` +
      "鉴权服务器地址填 https://<本站域名>/api/cdn/auth（不带任何参数）；" +
      "自定义参数：需要配置URL参数：①选择参数 → remote_addr → $remote_addr   " +
      "②选择参数 → request_uri → $request_uri",
  );
}

/** 实测通道的展示文案：由本次真正请求的 URL 归属决定，而不是配置推测。 */
const channel = computed<{ label: string; hint: string } | null>(() => {
  switch (probeSession.value?.mode as DeliveryMode | undefined) {
    case "direct":
      return { label: "直链加密流量", hint: "浏览器直连 123 云盘取密文，本地解密" };
    case "proxy":
      return { label: "中转加密流量", hint: "服务器纯反向代理密文，本地解密" };
    case "proxy_decrypt":
      return { label: "中转解密流量", hint: "服务器中转并解密，明文直达浏览器" };
    default:
      return null;
  }
});

const statItems = computed(() => [
  {
    label: "后端状态",
    value: status.value.ready ? "已就绪" : "未就绪",
    badge: true,
    tone: status.value.ready ? "badge--success" : "badge--danger",
  },
  {
    label: "直链签发",
    value: status.value.presignReady ? "可用" : "不可用",
    badge: true,
    tone: status.value.presignReady ? "badge--success" : "badge--warn",
  },
  {
    label: "直链空间",
    value: status.value.directLinkConfigured ? "已启用" : "未启用",
    badge: true,
    tone: status.value.directLinkConfigured ? "badge--success" : "",
  },
  {
    label: "中转解密",
    value: status.value.proxyDecrypt ? "开启" : "关闭",
    badge: true,
    tone: status.value.proxyDecrypt ? "badge--success" : "",
  },
  {
    label: "回源鉴权",
    value: status.value.authCallback ? "已开启" : "已关闭",
    badge: true,
    tone: status.value.authCallback ? "badge--success" : "",
  },
  {
    label: "账号凭据",
    value: status.value.credentialsConfigured ? "已配置" : "未配置",
    badge: true,
    tone: status.value.credentialsConfigured ? "badge--success" : "badge--warn",
  },
]);
</script>

<template>
  <AdminPage :error="error">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="loading" @click="load">刷新</AppButton>
      <AppButton size="sm" variant="primary" icon="activity" :loading="probing" @click="probe">
        连通性检查
      </AppButton>
    </Teleport>

    <div v-if="!status.ready" class="notice notice--warn">
      存储后端当前不可用：{{ status.detail || "没有更多说明" }}。凭据在「系统配置 → 123 云盘」分区填写，保存即生效。
      <div class="notice__actions">
        <AppButton size="sm" icon="settings" @click="router.push({ name: 'admin-settings' })">
          去填写凭据
        </AppButton>
      </div>
    </div>

    <StatStrip :items="statItems" />

    <Panel title="后端详情">
      <dl class="kv">
        <dt>类型</dt>
        <dd class="mono">{{ status.kind || "—" }}</dd>
        <dt>说明</dt>
        <dd>{{ status.detail || "—" }}</dd>
        <dt>根目录 ID</dt>
        <dd class="mono">{{ status.rootDirId || "—" }}</dd>
        <dt>更新时间</dt>
        <dd>{{ formatTime(status.updatedAt) }}</dd>
      </dl>
    </Panel>

    <Panel title="连通性自检">
      <div class="stack">
        <p class="faint">
          检查会上传一个探针小文件，由本页面直接向当前生效的下载通道发起真实请求，
          完成下载与内容校验后立即删除探针文件。
        </p>

        <div v-if="probing" class="notice">自检进行中……</div>
        <div v-else-if="probeStartError" class="notice notice--danger">
          探针准备失败：{{ probeStartError }}
        </div>

        <dl v-if="probeSession" class="kv probe">
          <dt>探针文件</dt>
          <dd class="mono">{{ probeSession.fileName }}</dd>
          <dt>当前下载通道</dt>
          <dd v-if="channel" class="probe--ok">
            {{ channel.label }}
            <span class="faint">（{{ channel.hint }}）</span>
          </dd>
          <dd v-else class="mono">{{ probeSession.mode }}</dd>
          <dt>下载</dt>
          <dd :class="verifyResult?.downloaded ? 'probe--ok' : 'probe--fail'">
            {{ verifyResult ? (verifyResult.downloaded ? "成功" : "失败") : "进行中…" }}
          </dd>
          <dt>内容校验</dt>
          <dd :class="verifyResult?.verified ? 'probe--ok' : 'probe--fail'">
            <template v-if="!verifyResult">进行中…</template>
            <template v-else-if="verifyResult.verified">
              一致{{ probeSession.mode === "proxy_decrypt" ? "（明文摘要）" : "（本地解密后摘要）" }}
            </template>
            <template v-else>不一致</template>
          </dd>
          <dt>测试后删除</dt>
          <dd :class="cleaned === true ? 'probe--ok' : cleaned === false ? 'probe--fail' : ''">
            {{ cleaned === true ? "已删除" : cleaned === false ? "删除失败" : "等待清理…" }}
          </dd>
          <template v-if="verifyResult">
            <dt>实测字节 / 耗时</dt>
            <dd class="mono">{{ verifyResult.bytesGot }} B · {{ verifyResult.latencyMs }} ms</dd>
          </template>
        </dl>

        <div v-if="verifyResult && !verifyResult.ok" class="notice notice--danger">
          自检未通过{{ verifyResult.detail ? `：${verifyResult.detail}` : "" }}
        </div>
        <div v-if="verifyResult?.ok && cleaned" class="notice notice--success">
          自检通过：上传、下载、内容校验与清理全部成功。
        </div>
        <div v-if="finishError" class="notice notice--danger">
          探针清理失败：{{ finishError }}。远端对象与临时票据可能残留，稍后可重试自检。
        </div>

        <p v-if="!probing && !probeSession && !probeStartError" class="faint">
          尚未执行，点击右上角「连通性检查」。
        </p>
      </div>
    </Panel>
  </AdminPage>
</template>

<style scoped>
.notice__actions {
  margin-top: var(--sp-2);
}
.probe {
  margin-top: var(--sp-2);
}
.probe--ok {
  color: var(--color-success, #15803d);
  font-weight: 600;
}
.probe--fail {
  color: var(--color-danger, #b91c1c);
  font-weight: 600;
}
</style>
