<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppDateTimePicker from "@/components/ui/AppDateTimePicker.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import FolderPicker from "@/components/FolderPicker.vue";
import FormField from "@/components/ui/FormField.vue";
import { apiKeyApi, downloadAPIKeySDK } from "@/api/endpoints";
import {
  API_KEY_SCOPE_DEFAULT,
  API_KEY_SCOPE_OPTIONS,
  apiKeyScopeText,
  hasPerm,
  type APIKey,
} from "@/api/types";
import { copyText, describeError, logError, useAsync } from "@/lib/async";
import { dateTimeLocalToUnix, formatTime } from "@/lib/format";
import { useSession } from "@/stores/session";
import { topbarSlot } from "@/stores/shell";
import { useToasts } from "@/stores/toast";

// API Key 管理。
//
// 页面只负责"签发、辨认、回收"三件事：令牌明文只在创建响应里出现一次，
// 列表永远只能看到前缀。SDK 下载挂在同一页，因为用户拿到 Key 之后的
// 下一步就是找怎么调用。

const session = useSession();
const toasts = useToasts();

const {
  data,
  loading: listLoading,
  error: listError,
  run: loadKeys,
} = useAsync(() => apiKeyApi.list(), { items: [] as APIKey[] });

onMounted(() => {
  if (session.apiEnabled.value) {
    void loadKeys();
  }
});

interface KeyRow {
  key: APIKey;
  workspaceLabel: string;
  scopeLabel: string;
  statusLabel: string;
  statusClass: string;
  lastUsedLabel: string;
  expiresLabel: string;
  createdLabel: string;
}

const rows = computed<KeyRow[]>(() => {
  const now = Math.floor(Date.now() / 1000);
  return data.value.items.map((key) => {
    const expired = key.expiresAt > 0 && key.expiresAt <= now;
    return {
      key,
      workspaceLabel: workspaceLabel(key.workspacePath),
      scopeLabel: apiKeyScopeText(key.scopes),
      statusLabel: expired ? "已过期" : "有效",
      statusClass: expired ? "badge--warn" : "badge--success",
      lastUsedLabel: key.lastUsedAt > 0 ? formatTime(key.lastUsedAt) : "从未使用",
      expiresLabel: key.expiresAt > 0 ? formatTime(key.expiresAt) : "永久",
      createdLabel: formatTime(key.createdAt),
    };
  });
});

function workspaceLabel(path: string): string {
  return path && path !== "/" ? path : "整个账号";
}

const columns: Column[] = [
  { key: "name", label: "名称", mobile: "title" },
  { key: "workspaceLabel", label: "工作区", width: "14rem" },
  { key: "scopeLabel", label: "权限", width: "15rem" },
  { key: "prefix", label: "前缀", width: "9rem" },
  { key: "statusLabel", label: "状态", width: "6rem" },
  { key: "lastUsedLabel", label: "最近使用", width: "9.5rem" },
  { key: "expiresLabel", label: "有效期", width: "9.5rem" },
  { key: "createdLabel", label: "创建时间", width: "9.5rem" },
  { key: "actions", label: "操作", align: "right", width: "6rem" },
];

// ---------------------------------------------------------------- 新建

const createOpen = ref(false);
const pickerOpen = ref(false);
const saving = ref(false);
const formError = ref("");
const form = reactive({ name: "", workspacePath: "", scopes: 0, expiresAtLocal: "" });
/** 创建成功后的明文令牌；非空时弹窗切换到"只显示一次"的交付面板。 */
const createdToken = ref("");
const createdWorkspace = ref("");
const createdScopes = ref(0);

/** 当前用户组实际开放的能力；Key 只能在这些能力里收窄。 */
const availableScopes = computed(() =>
  API_KEY_SCOPE_OPTIONS.reduce(
    (mask, option) =>
      (session.state.permissions & option.permMask) !== 0 ? mask | option.bit : mask,
    0,
  ),
);

/** 默认勾选上传、下载、文件管理、分享与直链；组里一项都没有时退回全部可用能力。 */
const defaultScopes = computed(() => {
  const preferred = API_KEY_SCOPE_DEFAULT & availableScopes.value;
  return preferred || availableScopes.value;
});

const scopeOptions = computed(() =>
  API_KEY_SCOPE_OPTIONS.map((option) => ({
    ...option,
    disabled: (availableScopes.value & option.bit) === 0,
  })),
);

const createdScopeLabel = computed(() => apiKeyScopeText(createdScopes.value));

function toggleScope(bit: number, on: boolean): void {
  form.scopes = on ? form.scopes | bit : form.scopes & ~bit;
}

function openCreate(): void {
  form.name = "";
  form.workspacePath = "";
  form.scopes = defaultScopes.value;
  form.expiresAtLocal = "";
  formError.value = "";
  createdToken.value = "";
  createdWorkspace.value = "";
  createdScopes.value = 0;
  pickerOpen.value = false;
  createOpen.value = true;
}

function closeCreate(): void {
  if (saving.value) {
    return;
  }
  pickerOpen.value = false;
  createOpen.value = false;
  createdToken.value = "";
  createdScopes.value = 0;
}

function selectWorkspace(path: string): void {
  form.workspacePath = path;
  pickerOpen.value = false;
  formError.value = "";
}

async function submit(): Promise<void> {
  const name = form.name.trim();
  const workspace = form.workspacePath.trim();
  if (!name) {
    formError.value = "请填写 Key 名称";
    return;
  }
  if (!workspace) {
    formError.value = "请选择 Key 的工作区";
    return;
  }
  if (!form.scopes) {
    formError.value = "请至少选择一项权限";
    return;
  }
  if ((form.scopes & ~availableScopes.value) !== 0) {
    formError.value = "选择的部分权限当前用户组未开放";
    return;
  }
  saving.value = true;
  formError.value = "";
  try {
    const created = await apiKeyApi.create({
      name,
      workspacePath: workspace,
      scopes: form.scopes,
      expiresAt: dateTimeLocalToUnix(form.expiresAtLocal),
    });
    createdToken.value = created.token;
    createdWorkspace.value = created.key.workspacePath;
    createdScopes.value = created.key.scopes;
    toasts.success("API Key 已创建");
    await loadKeys();
  } catch (err) {
    formError.value = describeError(err);
    logError("apiKeys.create", err);
  } finally {
    saving.value = false;
  }
}

async function copyToken(): Promise<void> {
  if (!createdToken.value) {
    return;
  }
  if (await copyText(createdToken.value)) {
    toasts.success("已复制");
  } else {
    toasts.error("复制失败", "请手动选中令牌文本");
  }
}

// ---------------------------------------------------------------- 删除

const removing = ref<APIKey | null>(null);
const removeBusy = ref(false);

async function confirmRemove(): Promise<void> {
  if (!removing.value) {
    return;
  }
  removeBusy.value = true;
  try {
    await apiKeyApi.remove(removing.value.id);
    toasts.success("API Key 已删除");
    removing.value = null;
    await loadKeys();
  } catch (err) {
    toasts.error("删除失败", describeError(err));
    logError("apiKeys.remove", err);
  } finally {
    removeBusy.value = false;
  }
}

// ---------------------------------------------------------------- SDK

const sdkBusy = ref(false);

async function downloadSdk(): Promise<void> {
  sdkBusy.value = true;
  try {
    const { blob, filename } = await downloadAPIKeySDK();
    const url = URL.createObjectURL(blob);
    const link = document.createElement("a");
    link.href = url;
    link.download = filename;
    document.body.appendChild(link);
    link.click();
    link.remove();
    window.setTimeout(() => URL.revokeObjectURL(url), 1000);
  } catch (err) {
    toasts.error("下载失败", describeError(err));
    logError("apiKeys.sdk", err);
  } finally {
    sdkBusy.value = false;
  }
}
</script>

<template>
  <div v-if="session.apiEnabled.value" class="stack">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="listLoading" @click="loadKeys()">刷新</AppButton>
      <AppButton size="sm" icon="download" :loading="sdkBusy" @click="downloadSdk">
        下载示例 SDK
      </AppButton>
      <AppButton size="sm" variant="primary" icon="plus" @click="openCreate">新建 Key</AppButton>
    </Teleport>

    <p v-if="listError" class="notice notice--danger">{{ listError }}</p>

    <div v-if="listLoading" class="card">
      <span class="spinner" /> 正在加载
    </div>
    <div v-else-if="rows.length === 0 && !listError" class="card">
      <AppEmpty title="暂无 API Key" />
    </div>
    <AppTable v-else :columns="columns" :rows="rows" :loading="listLoading">
      <template #name="{ row }">
        <div class="cell-main">
          <span class="cell-name" :title="row.key.name">{{ row.key.name }}</span>
        </div>
      </template>
      <template #workspaceLabel="{ row }">
        <span class="workspace-cell mono" :title="row.workspaceLabel">{{ row.workspaceLabel }}</span>
      </template>
      <template #scopeLabel="{ row }">
        <span class="scope-cell" :title="row.scopeLabel">{{ row.scopeLabel }}</span>
      </template>
      <template #prefix="{ row }">
        <span class="mono">{{ row.key.prefix }}…</span>
      </template>
      <template #statusLabel="{ row }">
        <span class="badge" :class="row.statusClass">{{ row.statusLabel }}</span>
      </template>
      <template #actions="{ row }">
        <div class="actions">
          <AppButton size="sm" variant="danger" icon="trash" @click="removing = row.key">
            删除
          </AppButton>
        </div>
      </template>
    </AppTable>

    <AppModal
      :open="createOpen"
      :title="createdToken ? '保存你的 API Key' : '新建 API Key'"
      :dismissible="!saving"
      @close="closeCreate"
    >
      <div v-if="createdToken" class="stack">
        <p class="notice notice--warn">
          这是令牌唯一一次显示。关闭后只能看到前缀，无法再取回；如果丢失，请删除后重新创建。
        </p>
        <div class="token-row">
          <code class="token-box mono">{{ createdToken }}</code>
          <AppButton icon="copy" @click="copyToken">复制</AppButton>
        </div>
        <p class="faint">
          <template v-if="createdWorkspace === '/'">这枚 Key 可以访问整个账号。</template>
          <template v-else>
            这枚 Key 只能访问 <code class="mono">{{ createdWorkspace }}</code> 及其子目录。
          </template>
          SDK 从环境变量 <span class="mono">XIAOYU_API_KEY</span> 读取令牌，
          不要把令牌写进代码或配置仓库。
        </p>
        <p class="faint">权限：{{ createdScopeLabel }}。创建后不可更改。</p>
      </div>
      <form v-else id="api-key-form" class="stack" @submit.prevent="submit">
        <FormField label="名称" hint="用于区分用途，例如「自动备份」或「脚本上传」">
          <input
            v-model="form.name"
            aria-label="Key 名称"
            class="input"
            maxlength="64"
            placeholder="自动备份"
          />
        </FormField>
        <FormField
          label="工作区"
          required
          hint="Key 只能访问所选目录及其子目录；选择根目录表示整个账号，创建后不可更改"
        >
          <div class="workspace-pick">
            <span class="workspace-pick__path" :title="form.workspacePath">
              <code v-if="form.workspacePath" class="mono">{{ form.workspacePath }}</code>
              <span v-else class="faint">尚未选择文件夹</span>
            </span>
            <AppButton size="sm" icon="folder" @click="pickerOpen = true">
              {{ form.workspacePath ? "更改" : "选择文件夹" }}
            </AppButton>
          </div>
        </FormField>
        <FormField
          label="权限"
          required
          hint="默认勾选上传、下载、删除与文件管理、分享、直链；只能授予用户组已开放的能力。"
        >
          <div class="scope-grid">
            <label
              v-for="option in scopeOptions"
              :key="option.bit"
              class="check"
              :class="{ 'scope-option--disabled': option.disabled }"
            >
              <input
                type="checkbox"
                :disabled="option.disabled"
                :checked="hasPerm(form.scopes, option.bit)"
                @change="toggleScope(option.bit, ($event.target as HTMLInputElement).checked)"
              />
              <span class="check__text">
                <span>{{ option.label }}</span>
                <span v-if="option.disabled" class="faint">用户组未开放</span>
              </span>
            </label>
          </div>
          <p v-if="availableScopes === 0" class="field__hint">当前用户组没有可授予的权限。</p>
        </FormField>
        <FormField label="有效期" hint="留空表示永久有效；到期后 Key 自动失效">
          <AppDateTimePicker v-model="form.expiresAtLocal" />
        </FormField>
        <p v-if="formError" class="notice notice--danger">{{ formError }}</p>
      </form>

      <template #footer>
        <AppButton v-if="createdToken" variant="primary" @click="closeCreate">我已保存</AppButton>
        <template v-else>
          <AppButton :disabled="saving" @click="closeCreate">取消</AppButton>
          <AppButton type="submit" form="api-key-form" variant="primary" :loading="saving">
            创建
          </AppButton>
        </template>
      </template>
    </AppModal>

    <FolderPicker
      :open="pickerOpen"
      title="选择工作区"
      initial-path="/"
      @select="selectWorkspace"
      @close="pickerOpen = false"
    />

    <ConfirmDialog
      :open="!!removing"
      danger
      title="删除 API Key"
      confirm-text="删除"
      :loading="removeBusy"
      message="删除后这枚 Key 立即失效，正在使用它的脚本会收到 403。此操作不可撤销。"
      :detail="removing?.name ?? ''"
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
  width: clamp(10rem, 16vw, 20rem);
  max-width: 100%;
}

.cell-name {
  display: block;
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}

.workspace-cell,
.scope-cell {
  display: block;
  max-width: 100%;
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}

.scope-grid {
  display: grid;
  gap: var(--sp-1) var(--sp-4);
  grid-template-columns: repeat(auto-fit, minmax(min(11rem, 100%), 1fr));
}

.scope-option--disabled {
  color: var(--c-text-muted);
  cursor: not-allowed;
}

/* 列宽只是表格的建议值；长权限列表会把「权限」列撑开，使省略号失效。
   固定单元格内容宽度后，表格才按预期收窄并在悬停时给出完整内容。 */
@media (min-width: 721px) {
  .workspace-cell {
    width: 14rem;
  }

  .scope-cell {
    width: 15rem;
  }
}

.workspace-pick {
  display: flex;
  align-items: center;
  gap: 0.5rem;
}

.workspace-pick__path {
  flex: 1;
  min-width: 0;
  display: flex;
  padding: 0.55rem 0.7rem;
  border: 1px solid var(--c-border);
  border-radius: var(--r-sm);
  background: var(--c-surface);
  font-size: var(--fs-sm);
}

.workspace-pick__path code {
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}

.actions {
  display: flex;
  gap: 0.4rem;
  justify-content: flex-end;
}

.token-row {
  display: flex;
  gap: 0.5rem;
  align-items: stretch;
}

.token-box {
  flex: 1;
  min-width: 0;
  padding: 0.7rem 0.8rem;
  border: 1px solid var(--c-border-strong);
  border-radius: 0.4rem;
  background: var(--c-surface);
  word-break: break-all;
  user-select: all;
}

@media (max-width: 720px) {
  .cell-main { width: 100%; }
  .token-row { flex-direction: column; align-items: stretch; }
  .workspace-pick { flex-direction: column; align-items: stretch; }
}
</style>
