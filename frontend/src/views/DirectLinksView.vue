<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from "vue";
import { useRoute, useRouter } from "vue-router";
import AppButton from "@/components/ui/AppButton.vue";
import AppDateTimePicker from "@/components/ui/AppDateTimePicker.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppPagination from "@/components/ui/AppPagination.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import FormField from "@/components/ui/FormField.vue";
import { directLinkApi, directLinkUrl } from "@/api/endpoints";
import { Perm, type DirectLink } from "@/api/types";
import { copyText, describeError, logError, useAsync } from "@/lib/async";
import { dateTimeLocalToUnix, formatTime, unixToDateTimeLocal } from "@/lib/format";
import { useSession } from "@/stores/session";
import { topbarSlot } from "@/stores/shell";
import { useToasts } from "@/stores/toast";

// 直链管理。直链是「分享」的直链版本，界面上刻意与分享并排而不是塞进
// 分享页：两者的出口完全不同——分享在浏览器本地解密、服务端不出流量，
// 直链由服务器中转解密，全部下行流量走本站。混在一处会让用户在毫不知情
// 的情况下把服务器带宽送出去。

const route = useRoute();
const router = useRouter();
const session = useSession();
const toasts = useToasts();

const canCreate = computed(() => session.directLinksEnabled.value && session.state.authenticated && session.can(Perm.DirectLink));

const limit = ref(50);
const offset = ref(0);

const {
  data,
  loading: listLoading,
  error: listError,
  run: loadLinks,
} = useAsync(() => directLinkApi.list(limit.value, offset.value), { items: [] as DirectLink[] });

watch([limit, offset], () => {
  void loadLinks();
});

/** 列表接口不回总数，只能用"本页取满 limit 条"推断后面还有。 */
const hasMore = computed(() => data.value.items.length >= limit.value);

function changeLimit(next: number): void {
  limit.value = next;
  offset.value = 0;
}

interface LinkRow {
  link: DirectLink;
  fileLabel: string;
  statusLabel: string;
  statusClass: string;
  hitsLabel: string;
  expiresLabel: string;
  createdLabel: string;
  /** 交付不了时给出具体原因，而不是让人复制一条必然失败的链接。 */
  blocked: string;
}

const now = ref(Date.now());
const rows = computed<LinkRow[]>(() => data.value.items.map(toRow));
let expiryTimer: ReturnType<typeof setInterval> | undefined;
onUnmounted(() => clearInterval(expiryTimer));

function toRow(link: DirectLink): LinkRow {
  // expiresAt 缺省与 0 都表示"永久"，先归一再用。
  const expiresAt = link.expiresAt ?? 0;
  const expired = expiresAt > 0 && expiresAt <= Math.floor(now.value / 1000);

  // disabled 与 targetAvailable 是两件事：前者是"这条链接还开不开放"，
  // 后者是"绑定的内容还在不在"。用户删除文件后两者会分叉——链接已停用，
  // 但内容在归档暂存期间仍然可恢复。把两者合并成一个状态会让用户以为
  // 自己删错了文件。
  let statusLabel = "正常";
  let statusClass = "badge--success";
  let blocked = "";
  if (link.disabled) {
    statusLabel = "已停用";
    statusClass = "badge--danger";
    blocked = link.targetAvailable ? "" : "链接已停用，且绑定内容不可用";
  } else if (expired) {
    statusLabel = "已过期";
    statusClass = "badge--warn";
  } else if (link.maxHits > 0 && link.hits >= link.maxHits) {
    statusLabel = "次数已耗尽";
    statusClass = "badge--warn";
  } else if (!link.targetAvailable) {
    statusLabel = "内容不可用";
    statusClass = "badge--danger";
    blocked = "绑定的文件已被删除或拉黑，这条链接交付不出内容";
  }

  return {
    link,
    fileLabel: link.sourceName || link.sourcePath || link.checksum.slice(0, 12),
    statusLabel,
    statusClass,
    blocked,
    hitsLabel: link.maxHits > 0 ? `${link.hits} / ${link.maxHits}` : String(link.hits),
    expiresLabel: expiresAt > 0 ? formatTime(expiresAt) : "永久",
    createdLabel: formatTime(link.createdAt),
  };
}

const columns: Column[] = [
  { key: "fileLabel", label: "文件", mobile: "title" },
  { key: "statusLabel", label: "状态", width: "6.5rem" },
  { key: "hitsLabel", label: "已用 / 上限", width: "6.5rem" },
  { key: "expiresLabel", label: "有效期", width: "9.5rem" },
  { key: "createdLabel", label: "创建时间", width: "9.5rem" },
  { key: "actions", label: "操作", align: "right", width: "15rem" },
];

async function copyLink(link: DirectLink): Promise<void> {
  const ok = await copyText(directLinkUrl(link.id));
  if (ok) {
    toasts.success("直链已复制");
  } else {
    toasts.error("复制失败", "浏览器不允许自动复制，请手动选中地址栏中的链接");
  }
}

/** 直链 → 文件：跳回文件页看这份内容。 */
async function openTarget(link: DirectLink): Promise<void> {
  try {
    const { path } = await directLinkApi.target(link.id);
    const parent = path.slice(0, path.lastIndexOf("/")) || "/";
    await router.push({ name: "files", params: { path: parent.split("/").filter(Boolean) }, query: { select: path } });
  } catch (error) {
    toasts.error("无法定位文件", describeError(error));
  }
}

// ---------------------------------------------------------------- 新建 / 编辑

const formOpen = ref(false);
const editing = ref<DirectLink | null>(null);
const saving = ref(false);
const formError = ref("");

const form = reactive({
  path: "",
  expiresAtLocal: "",
  maxHits: "0",
  disabled: false,
});

function openCreate(path: string): void {
  editing.value = null;
  Object.assign(form, { path, expiresAtLocal: "", maxHits: "0", disabled: false });
  formError.value = "";
  formOpen.value = true;
}

function openEdit(link: DirectLink): void {
  editing.value = link;
  Object.assign(form, {
    path: link.sourcePath ?? "",
    expiresAtLocal: unixToDateTimeLocal(link.expiresAt),
    maxHits: String(link.maxHits),
    disabled: link.disabled,
  });
  formError.value = "";
  formOpen.value = true;
}

async function submit(): Promise<void> {
  if (saving.value || !session.directLinksEnabled.value) return;
  if (!editing.value && !form.path.trim()) { formError.value = "请输入目标文件路径"; return; }
  const expiresAt = dateTimeLocalToUnix(form.expiresAtLocal);
  const rawMaxHits = form.maxHits.trim();
  const maxHits = rawMaxHits === "" ? 0 : Number(rawMaxHits);
  if ((rawMaxHits !== "" && !/^\d+$/.test(rawMaxHits)) || !Number.isSafeInteger(maxHits)) {
    formError.value = "次数上限必须是非负整数";
    return;
  }
  formError.value = "";
  saving.value = true;
  try {
    if (editing.value) {
      await directLinkApi.update(editing.value.id, {
        expiresAt,
        maxHits,
        disabled: form.disabled,
        expectUpdatedAt: editing.value.updatedAt,
      });
      toasts.success("直链已更新");
    } else {
      const created = await directLinkApi.create({
        path: form.path.trim(),
        expiresAt,
        maxHits,
      });
      const copied = await copyText(directLinkUrl(created.link.id));
      toasts.success(copied ? "直链已创建并复制" : "直链已创建");
    }
    formOpen.value = false;
    if (!editing.value && offset.value > 0) offset.value = 0;
    else await loadLinks();
  } catch (error) {
    formError.value = describeError(error);
  } finally {
    saving.value = false;
  }
}

// ---------------------------------------------------------------- 删除

const removing = ref<DirectLink | null>(null);
const removeBusy = ref(false);

function askRemove(link: DirectLink): void {
  removing.value = link;
}

async function confirmRemove(): Promise<void> {
  if (!removing.value) {
    return;
  }
  removeBusy.value = true;
  try {
    await directLinkApi.remove(removing.value.id);
    toasts.success("直链已删除");
    removing.value = null;
    await loadLinks();
  } catch (error) {
    logError("删除直链失败", error);
    toasts.error("删除失败", describeError(error));
  } finally {
    removeBusy.value = false;
  }
}

// 文件页点「创建直链」带 ?create= 过来，与分享页同一套交接方式。
onMounted(() => {
  if (!session.directLinksEnabled.value) return;
  void loadLinks();
  const create = typeof route.query.create === "string" ? route.query.create : "";
  expiryTimer = setInterval(() => { now.value = Date.now(); }, 1000);
  if (create && canCreate.value) {
    openCreate(create);
    void router.replace({ query: {} });
  }
});
</script>

<template>
  <div v-if="session.directLinksEnabled.value" class="stack">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="listLoading" @click="loadLinks()">刷新</AppButton>
      <AppButton v-if="canCreate" size="sm" variant="primary" icon="plus" @click="openCreate('')">
        新建直链
      </AppButton>
    </Teleport>

    <p v-if="listError" class="notice notice--danger">
      {{ listError }}
    </p>
    <p
      v-else-if="session.state.authenticated && !session.can(Perm.DirectLink)"
      class="notice notice--info"
    >
      当前用户组没有创建直链的权限，已有直链仍可管理。
    </p>

    <div v-if="listLoading" class="card">
      <span class="spinner" /> 正在加载
    </div>
    <div v-else-if="rows.length === 0 && !listError" class="card">
      <AppEmpty
        v-if="offset > 0"
        title="没有更多直链了"
        hint="已经翻到最后一页，把「每页条数」调小或用「上一页」往回翻。"
      />
      <AppEmpty
        v-else-if="canCreate"
        title="还没有直链"
        hint="在文件页选中一个文件后点「直链」，或用右上角的「新建直链」。"
      />
      <AppEmpty v-else title="还没有直链" hint="当前用户组没有创建直链的权限。" />
    </div>
    <template v-else-if="rows.length > 0">
      <AppTable :columns="columns" :rows="rows">
        <template #fileLabel="{ row }">
          <div class="cell-main">
            <button
              v-if="session.can(Perm.Download) && row.link.targetAvailable"
              type="button"
              class="link-button file-label"
              :title="row.fileLabel"
              @click="openTarget(row.link)"
            >
              {{ row.fileLabel }}
            </button>
            <span v-else class="file-label" :title="row.fileLabel">{{ row.fileLabel }}</span>
            <small v-if="row.blocked" class="cell-note cell-note--danger">{{ row.blocked }}</small>
          </div>
        </template>
        <template #statusLabel="{ row }">
          <span class="badge" :class="row.statusClass">{{ row.statusLabel }}</span>
        </template>
        <template #actions="{ row }">
          <div class="actions">
            <AppButton size="sm" icon="copy" @click="copyLink(row.link)">复制</AppButton>
            <AppButton size="sm" icon="edit" @click="openEdit(row.link)">编辑</AppButton>
            <AppButton
              size="sm"
              variant="danger"
              icon="trash"
              @click="askRemove(row.link)"
            >
              删除
            </AppButton>
          </div>
        </template>
      </AppTable>

    </template>

    <AppPagination
      v-if="!listLoading"
      :total="rows.length"
      :limit="limit"
      :offset="offset"
      :exact="false"
      :has-more="hasMore"
      @update:limit="changeLimit"
      @update:offset="offset = $event"
    />

    <AppModal
      :open="formOpen"
      :title="editing ? '编辑直链' : '新建直链'"
      wide
      :dismissible="!saving"
      @close="formOpen = false"
    >
      <form id="direct-link-form" class="stack" @submit.prevent="submit">
        <p class="notice notice--info">
          直链是一段可以直接贴进 wget、播放器或第三方脚本的地址，拿到它的人就能取走明文文件。
          与分享不同，下载流量全部经过本站服务器。只支持单个文件，目录请用分享。
        </p>

        <FormField :label="editing ? '创建时路径' : '目标路径'" :hint="editing ? '此处为路径快照，移动和重命名不影响直链' : '只能选择自己名下的文件'">
          <input v-model="form.path" aria-label="目标路径" class="input" :disabled="!!editing" placeholder="/目录/文件.bin" />
        </FormField>

        <FormField label="有效期" hint="留空表示永久有效">
          <AppDateTimePicker v-model="form.expiresAtLocal" />
        </FormField>

        <FormField label="次数上限" hint="0 表示不限。每次 GET（含分段请求）计一次，HEAD 探测不计次数">
          <input v-model="form.maxHits" aria-label="次数上限" class="input" inputmode="numeric" placeholder="0" />
        </FormField>

        <FormField v-if="editing" label="链接状态">
          <label class="checkbox">
            <input v-model="form.disabled" type="checkbox" />
            <span>停用这条直链（停用后立即失效，但记录保留）</span>
          </label>
        </FormField>

        <p v-if="formError" class="notice notice--danger">{{ formError }}</p>
      </form>

      <!-- #footer 必须是 AppModal 的直接子节点。把它塞进上面的 <form> 里，
           Vue 运行时仍会转发，但 vue-tsc 会因此放弃整个文件的模板绑定，
           连带把脚本里所有被模板引用的符号误报成「未使用」。 -->
      <template #footer>
        <AppButton :disabled="saving" @click="formOpen = false">取消</AppButton>
        <AppButton type="submit" form="direct-link-form" variant="primary" :loading="saving">
          {{ editing ? "保存" : "创建并复制链接" }}
        </AppButton>
      </template>
    </AppModal>

    <ConfirmDialog
      :open="!!removing"
      danger
      title="删除直链"
      confirm-text="删除"
      :loading="removeBusy"
      message="删除后这条地址立即失效，已经发出去的链接会返回 404。绑定文件本身不受影响。"
      :detail="removing ? removing.sourceName : ''"
      @cancel="removing = null"
      @confirm="confirmRemove"
    />
  </div>
</template>

<style scoped>
.cell-main {
  display: flex;
  flex-direction: column;
  gap: 0.15rem;
  min-width: 0;
  width: clamp(10rem, 15vw, 18rem);
  max-width: 100%;
  overflow-wrap: anywhere;
}

.file-label {
  display: block;
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}

@media (max-width: 720px) {
  .cell-main { width: 100%; }
}

.cell-note {
  font-size: 0.75rem;
  line-height: 1.35;
}

.cell-note--danger {
  color: var(--c-danger);
}

.link-button {
  border: 0;
  background: none;
  padding: 0;
  color: var(--c-accent);
  cursor: pointer;
  text-align: left;
  font: inherit;
  max-width: 100%;
  white-space: nowrap;
}

.link-button:hover {
  text-decoration: underline;
}

.actions {
  display: flex;
  gap: 0.4rem;
  justify-content: flex-end;
  flex-wrap: nowrap;
}
@media (max-width: 720px) {
  .actions { flex-wrap: wrap; }
}
</style>
