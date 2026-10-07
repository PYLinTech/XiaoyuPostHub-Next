<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { adminApi } from "@/api/endpoints";
import type { InviteCode, InviteUse } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import Panel from "@/components/admin/Panel.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppDateTimePicker from "@/components/ui/AppDateTimePicker.vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppPagination from "@/components/ui/AppPagination.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import FormField from "@/components/ui/FormField.vue";
import { topbarSlot } from "@/stores/shell";
import { copyText, describeError, logError, toastApiError, createRequestGate } from "@/lib/async";
import { dateTimeLocalToUnix, formatTime } from "@/lib/format";
import { useToasts } from "@/stores/toast";
import { useGroupOptions } from "@/composables/useGroupOptions";

// 邀请码。
//
// 明文码只在创建时返回一次：库里只存 HMAC 哈希，之后任何地方都取不回来。
// 因此创建成功后必须当场醒目展示，并且不能在列表里伪造一个"明文"列。

const toasts = useToasts();

const invites = ref<InviteCode[]>([]);
// 组下拉与显示名交给公共实现：UsersView / InvitesView 原本各抄了一份逐字相同的
// 副本（连注释都相同），却各自带了一套错误处理。这里刻意不取 groupsError：
// 建码只是选一个组名，读不到列表时默认组 normal 照样可用（后端默认也是 normal），
// 为此在界面上加一句警告没有意义。
const { groupOptions, groupLabel, loadGroups } = useGroupOptions("admin.invites");
const loading = ref(false);
const error = ref("");
const limit = ref(50);
const offset = ref(0);

const columns: Column[] = [
  { key: "codeHint", label: "邀请码", mobile: "title" },
  { key: "groupName", label: "目标组" },
  { key: "usage", label: "用量", align: "right" },
  { key: "status", label: "状态" },
  { key: "expiresAt", label: "有效期" },
  { key: "actions", label: "操作", align: "right" },
];

/**
 * 邀请码状态由数据推导，而不是只看 disabled 那一个布尔位。
 *
 * 用量满、已过期这两件事后端本来就会拒绝（ConsumeInviteCode 的 WHERE 条件），
 * 但库里不会为此改写 disabled——那会让"管理员主动停用"与"用完了"变成同一个
 * 状态，之后想把 max_uses 调大也救不回来。所以这里按事实推导：
 * 用尽 > 过期 > 停用 > 启用中——"用尽/过期"是事实，"停用"是意图，
 * 一条已经用尽的码最需要让人看见的是前者。
 */
function statusOf(invite: InviteCode): { label: string; tone: string } {
  if (invite.maxUses > 0 && invite.usedCount >= invite.maxUses) {
    return { label: "已用尽", tone: "badge--warn" };
  }
  const expiresAt = invite.expiresAt ?? 0;
  if (expiresAt > 0 && expiresAt <= Math.floor(Date.now() / 1000)) {
    return { label: "已过期", tone: "badge--warn" };
  }
  if (invite.disabled) {
    return { label: "已停用", tone: "" };
  }
  return { label: "启用中", tone: "badge--success" };
}

/**
 * 表格行的形状。
 *
 * 刻意声明具名类型而不是 `Record<string, unknown>`：那样写虽然也能让 AppTable
 * 编译通过，但插槽里的 `row.xxx` 全是 `unknown`，模板里只能靠 `String(row.x)`
 * 硬转，字段名写错编译器一句都不会说。
 *
 * 扁平层保留（它装的是展示口径：用量拼串、状态推导、时间的格式化），但行带上
 * 来源实体：操作入口因此可以直接收实体，不用再按 id 反查一次。
 */
interface InviteRow {
  /** 来源实体：启用/停用、删除、使用记录这几个入口都指向它。 */
  invite: InviteCode;
  codeHint: string;
  groupName: string;
  usage: string;
  disabled: boolean;
  statusLabel: string;
  statusTone: string;
  /** 用尽/过期的码已经不可用，再点"停用/启用"只是改一个不起作用的位置。 */
  toggleable: boolean;
  expiresAt: string;
  note: string;
  createdAt: string;
}

const rows = computed<InviteRow[]>(() =>
  invites.value.map((invite) => {
    const status = statusOf(invite);
    return {
      invite,
      codeHint: invite.codeHint,
      groupName: invite.groupName,
      usage: `${invite.usedCount} / ${invite.maxUses}`,
      disabled: invite.disabled,
      statusLabel: status.label,
      statusTone: status.tone,
      // 用尽/过期的码已经不可用，再点"停用/启用"只是改一个不起作用的位置。
      toggleable: status.label === "启用中" || status.label === "已停用",
      expiresAt: invite.expiresAt ? formatTime(invite.expiresAt) : "永久",
      note: invite.note || "—",
      createdAt: formatTime(invite.createdAt),
    };
  }),
);

const gate = createRequestGate();

async function load(): Promise<void> {
  // 序号守卫：并发请求里只有最新一次的结果可以写入界面。
  const token = gate.next();
  loading.value = true;
  error.value = "";
  try {
    const result = await adminApi.listInvites(limit.value, offset.value);
    if (!gate.isCurrent(token)) return;
    invites.value = result.items ?? [];
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    error.value = describeError(err);
    logError("admin.invites", err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

onMounted(() => {
  void load();
  void loadGroups();
});

// ---------------------------------------------------------------- 新建

const createOpen = ref(false);
const creating = ref(false);
const createError = ref("");
const issuedCode = ref("");

const form = reactive({
  groupName: "normal",
  maxUses: 1,
  expireLocal: "",
  note: "",
});

function openCreate(): void {
  form.groupName = "normal";
  form.maxUses = 1;
  form.expireLocal = "";
  form.note = "";
  createError.value = "";
  issuedCode.value = "";
  createOpen.value = true;
}

async function create(): Promise<void> {
  if (form.maxUses <= 0) {
    createError.value = "最大使用次数至少为 1";
    return;
  }
  creating.value = true;
  createError.value = "";
  try {
    const result = await adminApi.createInvite({
      groupName: form.groupName,
      maxUses: Math.trunc(form.maxUses),
      expiresAt: dateTimeLocalToUnix(form.expireLocal),
      note: form.note,
    });
    issuedCode.value = result.code;
    await load();
  } catch (err) {
    createError.value = describeError(err);
    logError("admin.invites.create", err);
  } finally {
    creating.value = false;
  }
}

async function copyCode(): Promise<void> {
  const ok = await copyText(issuedCode.value);
  if (ok) {
    toasts.success("邀请码已复制");
  } else {
    toasts.error("复制失败，请手动选中后复制");
  }
}

// ---------------------------------------------------------------- 启用 / 停用

const statusTarget = ref<InviteCode | null>(null);
const statusBusy = ref(false);

// 停用要二次确认：按钮本身就是 danger 档，而它挡掉的是"这枚码已经发出去、
// 有人正打算用它注册"——点错了只能再点回来，且中间那一小段时间里码是失效的。
// 启用是反方向（解除限制），因此直连执行，与 UsersView 的启用/停用分工一致。
// statusBusy 除了给确认窗做 loading，还用来锁住行内按钮：连点两下会在两次
// load 之间发出意图相反的两次请求，最终状态取决于谁后返回。
//
// 拆成两个函数而不是共用一个 toggleStatus：接口要的是"启用"，而实体字段是
// disabled，合并写就得靠取反推导目标状态，读代码的人要先在脑子里翻转一次。
// 分开后传进去的字面量就是目标状态，少一层出错余地。
function askDisableStatus(invite: InviteCode): void {
  statusTarget.value = invite;
}

async function confirmDisableStatus(): Promise<void> {
  const target = statusTarget.value;
  if (!target) {
    return;
  }
  statusBusy.value = true;
  try {
    await adminApi.setInviteStatus(target.id, false);
    toasts.success("邀请码已停用");
    statusTarget.value = null;
    await load();
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.invites.status", err);
  } finally {
    statusBusy.value = false;
  }
}

async function enableStatus(invite: InviteCode): Promise<void> {
  try {
    await adminApi.setInviteStatus(invite.id, true);
    toasts.success("邀请码已启用");
    await load();
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.invites.status", err);
  }
}

// ---------------------------------------------------------------- 删除

const deleteTarget = ref<InviteCode | null>(null);
const deleteBusy = ref(false);

function askDelete(invite: InviteCode): void {
  deleteTarget.value = invite;
}

async function confirmDelete(): Promise<void> {
  const target = deleteTarget.value;
  if (!target) {
    return;
  }
  deleteBusy.value = true;
  try {
    await adminApi.deleteInvite(target.id);
    toasts.success("邀请码已删除");
    deleteTarget.value = null;
    await load();
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.invites.delete", err);
  } finally {
    deleteBusy.value = false;
  }
}

// ---------------------------------------------------------------- 使用记录

const usesOpen = ref(false);
const usesLoading = ref(false);
const usesError = ref("");
const uses = ref<InviteUse[]>([]);

const useColumns: Column[] = [
  { key: "userAccount", label: "账号", mobile: "title" },
  { key: "clientIp", label: "来源 IP" },
  { key: "usedAt", label: "使用时间" },
];

const useRows = computed<Record<string, unknown>[]>(() =>
  uses.value.map((use) => ({
    userAccount: use.userAccount || `#${use.userId}`,
    clientIp: use.clientIp || "—",
    usedAt: formatTime(use.usedAt),
  })),
);

async function openUses(invite: InviteCode): Promise<void> {
  usesOpen.value = true;
  usesLoading.value = true;
  usesError.value = "";
  uses.value = [];
  try {
    const result = await adminApi.listInviteUses(invite.id);
    uses.value = result.items ?? [];
  } catch (err) {
    usesError.value = describeError(err);
    logError("admin.invites.uses", err);
  } finally {
    usesLoading.value = false;
  }
}
</script>

<template>
  <AdminPage :error="error">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="loading" @click="load()">刷新</AppButton>
      <AppButton size="sm" variant="primary" icon="key" @click="openCreate">新建邀请码</AppButton>
    </Teleport>

    <Panel flush>
      <AppTable
        :columns="columns"
        :rows="rows"
        :loading="loading"
        empty-title="没有邀请码"
        empty-hint="新建一个邀请码后，用户即可凭它注册到指定用户组。"
      >
          <template #codeHint="{ row }">
            <div class="icell">
              <span class="mono icell__code">•••• {{ row.codeHint }}</span>
              <span class="icell__meta truncate">{{ row.note !== "—" ? row.note : `创建于 ${row.createdAt}` }}</span>
            </div>
          </template>

          <template #groupName="{ row }">
            <span class="badge">{{ groupLabel(row.groupName) }}</span>
          </template>

          <template #usage="{ row }">
            <span class="mono">{{ row.usage }}</span>
          </template>

          <template #status="{ row }">
            <span class="badge" :class="row.statusTone">{{ row.statusLabel }}</span>
          </template>

          <template #expiresAt="{ row }">
            <span class="nowrap">{{ row.expiresAt }}</span>
          </template>

          <template #actions="{ row }">
            <div class="actions">
              <AppButton
                v-if="row.toggleable"
                size="sm"
                :icon="row.disabled ? 'play' : 'lock'"
                :variant="row.disabled ? 'default' : 'danger'"
                :title="row.disabled ? '启用该邀请码' : '停用该邀请码'"
                :disabled="statusBusy"
                @click="row.disabled ? enableStatus(row.invite) : askDisableStatus(row.invite)"
              />
              <AppButton size="sm" icon="list" title="使用记录" @click="openUses(row.invite)" />
              <AppButton
                size="sm"
                variant="danger"
                icon="trash"
                title="删除邀请码"
                @click="askDelete(row.invite)"
              />
            </div>
          </template>
        </AppTable>
      <div class="panel-foot">
        <AppPagination
          :total="offset + invites.length"
          :limit="limit"
          :offset="offset"
          :exact="false"
          @update:limit="(value: number) => { limit = value; offset = 0; load(); }"
          @update:offset="(value: number) => { offset = value; load(); }"
        />
      </div>
    </Panel>

    <AppModal
      :open="createOpen"
      :title="issuedCode ? '邀请码已生成' : '新建邀请码'"
      :dismissible="!issuedCode"
      @close="createOpen = false"
    >
      <div v-if="issuedCode" class="stack">
        <p class="notice notice--warn">关闭后无法再次查看，请立即复制并转交。</p>
        <div class="issued">
          <code class="issued__code">{{ issuedCode }}</code>
          <AppButton icon="copy" @click="copyCode">复制</AppButton>
        </div>
      </div>

      <div v-else class="stack">
        <div class="form-grid">
          <FormField label="目标组" required hint="注册成功后该账号会直接进入这个组。">
            <AppSelect v-model="form.groupName" aria-label="目标组" :options="groupOptions" />
          </FormField>

          <FormField label="最大使用次数" required hint="至少 1 次。">
            <input v-model.number="form.maxUses" class="input" type="number" min="1" step="1" />
          </FormField>

          <FormField label="有效期" hint="留空表示永久有效。">
            <AppDateTimePicker v-model="form.expireLocal" />
          </FormField>
        </div>

        <FormField label="备注" hint="仅管理员可见，用于记住发给谁。">
          <input v-model="form.note" class="input" />
        </FormField>

        <p v-if="createError" class="field__error">{{ createError }}</p>
      </div>

      <template #footer>
        <template v-if="issuedCode">
          <AppButton variant="primary" @click="createOpen = false">我已保存</AppButton>
        </template>
        <template v-else>
          <AppButton :disabled="creating" @click="createOpen = false">取消</AppButton>
          <AppButton variant="primary" :loading="creating" @click="create">生成</AppButton>
        </template>
      </template>
    </AppModal>

    <AppModal :open="usesOpen" wide title="使用记录" @close="usesOpen = false">
      <div class="stack">
        <div v-if="usesError" class="notice notice--danger">{{ usesError }}</div>
        <AppTable
          :columns="useColumns"
          :rows="useRows"
          :loading="usesLoading"
          empty-title="该邀请码尚未被使用"
          empty-hint="凭它注册成功后，这里会列出账号、来源 IP 与时间。"
        />
      </div>
    </AppModal>

    <ConfirmDialog
      :open="!!statusTarget"
      danger
      confirm-text="停用邀请码"
      :loading="statusBusy"
      :message="`停用邀请码 •••• ${statusTarget?.codeHint ?? ''}？`"
      detail="这枚码会立即不能再用于注册；如果已经发出去，持有者会当场注册失败。随时可以重新启用。"
      @confirm="confirmDisableStatus"
      @cancel="statusTarget = null"
    />

    <ConfirmDialog
      :open="!!deleteTarget"
      danger
      confirm-text="删除邀请码"
      :loading="deleteBusy"
      :message="`删除邀请码 •••• ${deleteTarget?.codeHint ?? ''}？`"
      detail="已用它注册的账号不受影响；删除后这枚码立即失效，且无法恢复。"
      @confirm="confirmDelete"
      @cancel="deleteTarget = null"
    />
  </AdminPage>
</template>

<style scoped>
.icell {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.icell__code {
  font-weight: 600;
  color: var(--c-text);
  letter-spacing: 0.08em;
}

.icell__meta {
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
}

.issued {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
  flex-wrap: wrap;
  padding: var(--sp-4);
  border: 1px dashed var(--c-border-strong);
  border-radius: var(--r-md);
  background: var(--c-surface-sunken);
}

.issued__code {
  font-family: var(--font-mono);
  font-size: var(--fs-lg);
  font-weight: 600;
  letter-spacing: 0.06em;
  word-break: break-all;
}
</style>
