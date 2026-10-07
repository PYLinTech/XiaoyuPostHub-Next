<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { adminApi } from "@/api/endpoints";
import type { Announcement, AnnouncementKind, Audience } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import Panel from "@/components/admin/Panel.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppDateTimePicker from "@/components/ui/AppDateTimePicker.vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import AppTable, { type Column } from "@/components/ui/AppTable.vue";
import AppTabs from "@/components/ui/AppTabs.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import FormField from "@/components/ui/FormField.vue";
import { topbarSlot } from "@/stores/shell";
import { createRequestGate, describeError, logError, toastApiError } from "@/lib/async";
import { dateTimeLocalToUnix, formatTime, unixToDateTimeLocal } from "@/lib/format";
import { useToasts } from "@/stores/toast";

// 公告与消息。
//
// 三种类型分开拉取而不是一次拉全部再在前端筛：三种的语义完全不同
// （滚动公告全局唯一、消息可定向到账号），混在一起列表几乎没有可读性。

const toasts = useToasts();

const tabs: { key: AnnouncementKind; label: string }[] = [
  { key: "ticker", label: "顶部滚动公告" },
  { key: "announcement", label: "消息类公告" },
  { key: "message", label: "普通消息" },
];

const KIND_OPTIONS = tabs.map((tab) => ({ value: tab.key, label: tab.label }));

const AUDIENCE_OPTIONS = [
  { value: "all", label: "全体（含访客）" },
  { value: "users", label: "指定用户" },
] as const;

const kind = ref<AnnouncementKind>("ticker");
const items = ref<Announcement[]>([]);
const loading = ref(false);
const error = ref("");

const kindLabel = computed(() => tabs.find((tab) => tab.key === kind.value)?.label ?? "公告");

// 滚动公告全局唯一且永远在顶部，"置顶"对它没有意义：列不展示、表单不可设。
const columns = computed<Column[]>(() => {
  const base: Column[] = [
    { key: "title", label: "标题", mobile: "title" },
    { key: "audience", label: "接收范围" },
    { key: "enabled", label: "状态" },
  ];
  if (kind.value !== "ticker") {
    base.push({ key: "pinned", label: "置顶" });
  }
  base.push(
    { key: "expireAt", label: "过期时间" },
    { key: "createdAt", label: "创建时间" },
    { key: "actions", label: "操作", align: "right" },
  );
  return base;
});

const rows = computed<Record<string, unknown>[]>(() =>
  items.value.map((item) => ({
    id: item.id,
    title: item.title,
    audience: item.audience === "all" ? "全体（含访客）" : `指定用户（${item.targets?.length ?? 0}）`,
    enabled: item.enabled,
    enabledLabel: item.enabled ? "启用" : "停用",
    pinned: item.pinned,
    expireAt: item.expireAt ? formatTime(item.expireAt) : "永久",
    createdAt: formatTime(item.createdAt),
  })),
);

const gate = createRequestGate();

async function load(): Promise<void> {
  // 序号守卫：切页签会连发请求，只有最新一次的结果可以写入界面。
  // 少了它，慢的那次后到会把表格刷成上一个页签的内容，而页签高亮已经切走了。
  const token = gate.next();
  loading.value = true;
  error.value = "";
  try {
    const result = await adminApi.listAnnouncements(kind.value);
    if (!gate.isCurrent(token)) return;
    items.value = result.items ?? [];
  } catch (err) {
    if (!gate.isCurrent(token)) return;
    error.value = describeError(err);
    logError("admin.announcements", err);
  } finally {
    if (gate.isCurrent(token)) loading.value = false;
  }
}

onMounted(load);

function switchKind(next: string): void {
  kind.value = next as AnnouncementKind;
  void load();
}

// ---------------------------------------------------------------- 编辑

const editorOpen = ref(false);
const saving = ref(false);
const formError = ref("");

const form = reactive({
  id: "",
  kind: "ticker" as AnnouncementKind,
  audience: "all" as Audience,
  title: "",
  body: "",
  enabled: true,
  pinned: false,
  expireLocal: "",
  targetsText: "",
});

function openCreate(): void {
  form.id = "";
  form.kind = kind.value;
  form.audience = "all";
  form.title = "";
  form.body = "";
  form.enabled = true;
  form.pinned = false;
  form.expireLocal = "";
  form.targetsText = "";
  formError.value = "";
  editorOpen.value = true;
}

function openEdit(item: Announcement): void {
  form.id = item.id;
  form.kind = item.kind;
  form.audience = item.audience;
  form.title = item.title;
  form.body = item.body;
  form.enabled = item.enabled;
  form.pinned = item.pinned;
  form.expireLocal = unixToDateTimeLocal(item.expireAt);
  form.targetsText = (item.targets ?? []).join(", ");
  formError.value = "";
  editorOpen.value = true;
}

function editById(id: unknown): void {
  const key = String(id);
  const target = items.value.find((item) => item.id === key);
  if (target) {
    openEdit(target);
  }
}

/** 逗号、空格、换行都当分隔符：管理员从表格里粘贴一列 ID 是常见操作。 */
function parseTargets(raw: string): { ids: number[]; error: string } {
  const parts = raw
    .split(/[\s,，;；]+/)
    .map((part) => part.trim())
    .filter(Boolean);
  const ids: number[] = [];
  for (const part of parts) {
    if (!/^\d+$/.test(part)) {
      return { ids: [], error: `「${part}」不是合法的用户 ID，只能填数字` };
    }
    const value = Number(part);
    if (value <= 0) {
      return { ids: [], error: "用户编号必须大于 0" };
    }
    ids.push(value);
  }
  return { ids, error: "" };
}

async function save(): Promise<void> {
  if (!form.title.trim()) {
    formError.value = "标题不得为空";
    return;
  }
  let targets: number[] | undefined;
  if (form.audience === "users") {
    const parsed = parseTargets(form.targetsText);
    if (parsed.error) {
      formError.value = parsed.error;
      return;
    }
    if (parsed.ids.length === 0) {
    formError.value = "接收范围选择「指定用户」时至少要填一个用户编号";
      return;
    }
    targets = parsed.ids;
  }

  saving.value = true;
  formError.value = "";
  try {
    await adminApi.saveAnnouncement({
      id: form.id || undefined,
      kind: form.kind,
      audience: form.audience,
      title: form.title.trim(),
      body: form.body,
      enabled: form.enabled,
      pinned: form.kind === "ticker" ? false : form.pinned,
      expireAt: dateTimeLocalToUnix(form.expireLocal),
      targets,
    });
    toasts.success(form.id ? "公告已更新" : "公告已创建");
    editorOpen.value = false;
    // 编辑时可能改了类型，切回当前页签重新拉取以免列表与内容脱节。
    await load();
  } catch (err) {
    formError.value = describeError(err);
    logError("admin.announcements.save", err);
  } finally {
    saving.value = false;
  }
}

// ---------------------------------------------------------------- 删除

const deleteTarget = ref<Announcement | null>(null);
const deleteBusy = ref(false);

function askDelete(id: unknown): void {
  deleteTarget.value = items.value.find((item) => item.id === String(id)) ?? null;
}

async function confirmDelete(): Promise<void> {
  const target = deleteTarget.value;
  if (!target) {
    return;
  }
  deleteBusy.value = true;
  try {
    await adminApi.deleteAnnouncement(target.id);
    toasts.success("公告已删除");
    deleteTarget.value = null;
    await load();
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.announcements.delete", err);
  } finally {
    deleteBusy.value = false;
  }
}
</script>

<template>
  <AdminPage :error="error">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="loading" @click="load()">刷新</AppButton>
      <AppButton size="sm" variant="primary" icon="plus" @click="openCreate">新建</AppButton>
    </Teleport>

    <AppTabs :tabs="tabs" :model-value="kind" @update:model-value="switchKind" />

    <p v-if="kind === 'ticker'" class="notice notice--info">
      顶部滚动公告全局仅一条：保存新的启用公告会自动停用旧的那条。
    </p>

    <Panel :title="kindLabel" :count="`${rows.length} 条`" flush>
      <AppTable
        :columns="columns"
        :rows="rows"
        :loading="loading"
        empty-title="没有记录"
        empty-hint="当前类型下还没有任何公告。"
      >
          <template #title="{ row }">
            <span class="truncate" :title="String(row.title)">{{ row.title }}</span>
          </template>

          <template #enabled="{ row }">
            <span class="badge" :class="row.enabled ? 'badge--success' : ''">{{ row.enabledLabel }}</span>
          </template>

          <template #pinned="{ row }">
            <span v-if="row.pinned" class="badge badge--accent">置顶</span>
            <span v-else class="faint">—</span>
          </template>

          <template #actions="{ row }">
            <div class="actions">
              <AppButton size="sm" icon="edit" @click="editById(row.id)">编辑</AppButton>
              <AppButton size="sm" variant="danger" icon="trash" @click="askDelete(row.id)">删除</AppButton>
            </div>
          </template>
        </AppTable>
    </Panel>

    <AppModal
      :open="editorOpen"
      wide
      :title="form.id ? '编辑公告' : '新建公告'"
      @close="editorOpen = false"
    >
      <div class="stack">
        <div class="form-grid">
          <FormField label="类型" required hint="滚动公告全局仅一条。">
            <AppSelect v-model="form.kind" aria-label="类型" :options="KIND_OPTIONS" />
          </FormField>

          <FormField label="接收范围" required>
            <AppSelect v-model="form.audience" aria-label="接收范围" :options="AUDIENCE_OPTIONS" />
          </FormField>

          <FormField label="过期时间" hint="留空表示永久有效。">
            <AppDateTimePicker v-model="form.expireLocal" />
          </FormField>
        </div>

        <FormField label="标题" required>
          <input v-model="form.title" class="input" />
        </FormField>

        <FormField label="正文">
          <textarea v-model="form.body" class="textarea" />
        </FormField>

        <div class="row">
          <label class="check">
            <input v-model="form.enabled" type="checkbox" />
            <span class="check__text">启用</span>
          </label>
          <label v-if="form.kind !== 'ticker'" class="check">
            <input v-model="form.pinned" type="checkbox" />
            <span class="check__text">置顶</span>
          </label>
        </div>

        <FormField
          v-if="form.audience === 'users'"
          label="用户编号列表"
          required
          hint="用逗号、空格或换行分隔。"
        >
          <textarea v-model="form.targetsText" class="textarea" placeholder="1, 2, 3" />
        </FormField>

        <p v-if="form.audience === 'users'" class="notice notice--info">
          仅列出的账号登录后可见。
        </p>

        <p v-if="formError" class="field__error">{{ formError }}</p>
      </div>

      <template #footer>
        <AppButton :disabled="saving" @click="editorOpen = false">取消</AppButton>
        <AppButton variant="primary" :loading="saving" @click="save">保存</AppButton>
      </template>
    </AppModal>

    <ConfirmDialog
      :open="!!deleteTarget"
      danger
      confirm-text="删除公告"
      :loading="deleteBusy"
      :message="`删除公告「${deleteTarget?.title ?? ''}」？`"
      detail="公告正文、定向接收者以及所有用户的已读记录会一并删除，删除后无法恢复。"
      @confirm="confirmDelete"
      @cancel="deleteTarget = null"
    />
  </AdminPage>
</template>
