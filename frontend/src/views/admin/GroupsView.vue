<script setup lang="ts">
import { computed, onMounted, reactive, ref } from "vue";
import { adminApi } from "@/api/endpoints";
import { PERM_LABELS, hasPerm, type GroupDetail, type GroupMailDomain } from "@/api/types";
import AdminPage from "@/components/admin/AdminPage.vue";
import AppButton from "@/components/ui/AppButton.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import ConfirmDialog from "@/components/ui/ConfirmDialog.vue";
import FormField from "@/components/ui/FormField.vue";
import { topbarSlot } from "@/stores/shell";
import { describeError, logError, toastApiError } from "@/lib/async";
import { formatBytes } from "@/lib/format";
import { refreshAfterAdminChange } from "@/stores/session";
import { useToasts } from "@/stores/toast";

// 用户组与配额。
//
// 组是寥寥几个信息量大的实体，用"每行一实体"的表格会被配额清单顶得臃肿，
// 窄屏卡片化后又变成一长串"字段名 + 值"。这里直接排成卡片网格：一张卡 =
// 一个组，自上而下三段——标识与操作、关键数字、配额清单。
//
// 收件域名也在这里：它是组的属性而不是独立实体（域名与组的绑定表
// 以组名 + 域名为键），另开一页只会让同一个字段有两个入口。

const toasts = useToasts();

const details = ref<GroupDetail[]>([]);
const loading = ref(false);
const error = ref("");

// 配额键就是后端 quota_counters / group_quotas 的维度名，取值以
// internal/store/groups.go 的 Quota* 常量为准，并由 internal/service/groups.go
// 的 knownQuotaKeys 做白名单校验——自创的键会被后端直接拒绝。
const QUOTA_META: Array<{ key: string; label: string; bytes: boolean; hint: string }> = [
  { key: "storage.total", label: "存储总量上限", bytes: true, hint: "该组所有用户的明文总占用" },
  { key: "file.max", label: "单文件上限", bytes: true, hint: "单次上传的明文大小" },
  // 口径必须写准：日流量的计数键是 DayKey(...) 即 UTC 日期（store/db.go），
  // 额度在 UTC 00:00 归零。写成"按自然日重置"会让东八区管理员以为归零点是
  // 本地 00:00，实际却是早上 8 点。
  { key: "traffic.daily.download", label: "每日下载流量", bytes: true, hint: "按 UTC 自然日重置（UTC 00:00 归零，非本地零点）" },
  { key: "count.pending_uploads", label: "并发上传上限", bytes: false, hint: "同时存在的上传会话数上限" },
  // 邮件配额与文件配额相互独立：收件域名在本组的「收件」一栏里设置，
  // 这两项决定每个用户能在这些域名下建多少地址、占多少邮件存储。
  { key: "count.mail_addresses", label: "每人邮箱地址数", bytes: false, hint: "单个用户可自助创建的邮箱地址数上限；0 = 禁止创建" },
  { key: "mail.storage.total", label: "邮件存储总量上限", bytes: true, hint: "该组所有用户邮件的明文总占用" },
];

/** 配额的结构化行：按 QUOTA_META 的固定顺序输出，每组都按同一顺序排，
    逐行扫下来不会被后端返回顺序的差别打乱。 */
interface QuotaLine {
  label: string;
  value: string;
}

function quotaLines(quotas: Record<string, number>): QuotaLine[] {
  const lines: QuotaLine[] = [];
  for (const meta of QUOTA_META) {
    const limitValue = quotas[meta.key];
    if (limitValue === undefined) {
      continue;
    }
    lines.push({
      label: meta.label,
      value: meta.bytes ? formatBytes(limitValue) : String(limitValue),
    });
  }
  return lines;
}

/** 响应里的配额数组转成键值表；rows 与编辑弹窗共用同一份转换。 */
function quotaRecord(detail: GroupDetail): Record<string, number> {
  const quotas: Record<string, number> = {};
  for (const quota of detail.quotas ?? []) {
    quotas[quota.quotaKey] = quota.limitValue;
  }
  return quotas;
}

/** 表格行的展示对象：定型给 AppTable 的泛型，具名插槽里的 row 就不用再断言。 */
interface GroupRow {
  name: string;
  displayName: string;
  isBuiltin: boolean;
  memberCount: number;
  permCount: number;
  priority: number;
  quotaLines: QuotaLine[];
  mailDomains: GroupMailDomain[];
}

const rows = computed<GroupRow[]>(() =>
  details.value.map((detail) => ({
    name: detail.group.name,
    displayName: detail.group.displayName,
    isBuiltin: detail.group.isBuiltin,
    memberCount: detail.memberCount,
    permCount: PERM_LABELS.filter((item) => hasPerm(detail.group.permissions, item.bit)).length,
    priority: detail.group.priority,
    quotaLines: quotaLines(quotaRecord(detail)),
    // 同一组可以托管多个域名，逐条展示：收件开关是按绑定算的，
    // 一格塞一个域名会漏掉其余域名的开关状态。
    mailDomains: detail.mailDomains ?? [],
  })),
);

async function load(): Promise<void> {
  loading.value = true;
  error.value = "";
  try {
    const result = await adminApi.listGroups();
    details.value = result.items ?? [];
  } catch (err) {
    error.value = describeError(err);
    logError("admin.groups", err);
  } finally {
    loading.value = false;
  }
}

onMounted(load);

// ---------------------------------------------------------------- 编辑弹窗

type PermItem = (typeof PERM_LABELS)[number];

/** 权限按 PERM_LABELS 的 group 字段分组展示，避免 15 个复选框铺成一长条。 */
const permGroups = computed(() => {
  const map = new Map<string, PermItem[]>();
  for (const item of PERM_LABELS) {
    const list = map.get(item.group);
    if (list) {
      list.push(item);
    } else {
      map.set(item.group, [item]);
    }
  }
  return Array.from(map, ([title, items]) => ({ title, items }));
});

/** 表格行是展示对象，按组名回到原始实体，避免在模板里做类型断言。 */
function detailOf(name: string): GroupDetail | undefined {
  return details.value.find((detail) => detail.group.name === name);
}

interface QuotaDraft {
  enabled: boolean;
  /** 字节类配额以字节存（后端契约），输入框里显示的是按所选单位换算后的数。 */
  value: number;
  unit: QuotaUnit;
}

/** 字节输入支持的两档单位；倍数与 formatBytes 同为 1024 进制。 */
type QuotaUnit = "MB" | "GB";
const UNIT_FACTORS: Record<QuotaUnit, number> = { MB: 1024 ** 2, GB: 1024 ** 3 };
const UNIT_OPTIONS: { value: QuotaUnit; label: string }[] = [
  { value: "MB", label: "MB" },
  { value: "GB", label: "GB" },
];

const editorOpen = ref(false);
const editing = ref(false);
const saving = ref(false);
const formError = ref("");

const form = reactive({
  name: "",
  displayName: "",
  priority: 0,
  permissions: 0,
});

/** 「收件」区块的一行草稿。域名与组是多对多，一个组可以托管多个域名，
    每个域名还带自己的收件开关，因此这里是一个列表而不是单个字段。 */
interface ReceiveDomainDraft {
  domain: string;
  receiveEnabled: boolean;
  /** 打开编辑时来自接口：该域下【本组用户】的地址数。>0 时这一行不能移除。
      表单内新加的行还没有任何地址，记 -1，与"0 个地址"区分开。 */
  addressCount: number;
}

const receiveDomains = ref<ReceiveDomainDraft[]>([]);
/** 新增域名用的输入框。它不进列表，只有点了「添加」才成为一行。 */
const newDomain = ref("");

/** 域名格式：点分标签、每段只允许字母数字与连字符且不能以连字符开头结尾。
    与后端同一套写法，前端只挡明显写错的形态，完整校验仍在后端
    （那里的正则是最终口径），避免两处规则各说各话。 */
const DOMAIN_RE = /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$/;
/** 域名总长上限，与后端一致（DNS 名字的 255 字节上限留 2 字节给根标签点）。 */
const DOMAIN_MAX = 253;

const quotaDrafts = reactive<Record<string, QuotaDraft>>({});

/** 已有字节量选个合适的展示单位：够 1GB 用 GB，否则 MB。 */
function pickUnit(bytes: number): QuotaUnit {
  return bytes >= UNIT_FACTORS.GB ? "GB" : "MB";
}

function resetQuotaDrafts(quotas: Record<string, number>): void {
  for (const meta of QUOTA_META) {
    const current = quotas[meta.key];
    const value = current !== undefined && current > 0 ? current : 0;
    quotaDrafts[meta.key] = {
      enabled: current !== undefined,
      value,
      unit: meta.bytes ? pickUnit(value) : "MB",
    };
  }
}

/** 输入框显示的数字：字节量按所选单位换算，计数配额原样。 */
function quotaAmount(meta: (typeof QUOTA_META)[number]): number {
  const draft = quotaDrafts[meta.key];
  return meta.bytes ? draft.value / UNIT_FACTORS[draft.unit] : draft.value;
}

function setQuotaAmount(meta: (typeof QUOTA_META)[number], event: Event): void {
  const amount = Math.max(0, Number((event.target as HTMLInputElement).value || 0));
  quotaDrafts[meta.key].value = meta.bytes ? amount * UNIT_FACTORS[quotaDrafts[meta.key].unit] : amount;
}

// ---------------------------------------------------------------- 收件域名

/** 往列表里加一行。校验放在点「添加」时而不是保存时：
    写错的域名当场就能改，攒到保存才报错会让人以为整张表单都填错了。 */
function addDomain(): void {
  // 域名一律小写化后再入列表，这样保存时不必再逐行清洗，
  // 列表里显示的也就是真正会提交给后端的那个值。
  const domain = newDomain.value.trim().toLowerCase();
  if (!domain) {
    return;
  }
  formError.value = "";
  if (domain.length > DOMAIN_MAX || !DOMAIN_RE.test(domain)) {
    formError.value = "收件域名格式不正确";
    return;
  }
  // 同一个绑定在一张表单里出现两次没有意义，保存时后一份会盖掉前一份。
  if (receiveDomains.value.some((row) => row.domain === domain)) {
    formError.value = `该组已经绑定了 ${domain}`;
    return;
  }
  // 新加的域名默认开启收件：管理员要的是"能收到信"，暂停是一个需要理由的状态。
  receiveDomains.value.push({ domain, receiveEnabled: true, addressCount: -1 });
  newDomain.value = "";
}

/** 域名下面已有邮箱地址的行不能移除：地址是用户数据，
    解绑会让这些地址失去所属组，后端同样会拒绝。 */
function removeDomain(index: number): void {
  const row = receiveDomains.value[index];
  if (!row || row.addressCount > 0) {
    return;
  }
  receiveDomains.value.splice(index, 1);
}

function openCreate(): void {
  editing.value = false;
  form.name = "";
  form.displayName = "";
  form.priority = 0;
  form.permissions = 0;
  // 新建没有可继承的绑定：列表从空开始，管理员自己加。
  receiveDomains.value = [];
  newDomain.value = "";
  formError.value = "";
  resetQuotaDrafts({});
  editorOpen.value = true;
}

function openEdit(detail: GroupDetail): void {
  editing.value = true;
  form.name = detail.group.name;
  form.displayName = detail.group.displayName;
  form.priority = detail.group.priority;
  form.permissions = detail.group.permissions;
  // 复制成草稿而不是直接用 detail 里的对象：管理员在表单上的每一次
  // 改动都只落在草稿上，点「取消」才不会把列表页的数据改脏。
  receiveDomains.value = (detail.mailDomains ?? []).map((item) => ({
    domain: item.domain,
    receiveEnabled: item.receiveEnabled,
    addressCount: item.addressCount,
  }));
  newDomain.value = "";
  formError.value = "";
  resetQuotaDrafts(quotaRecord(detail));
  editorOpen.value = true;
}

/** 只能按位运算切换：用加减法会在重复点击时进位，把相邻权限位一起改掉。 */
function togglePerm(bit: number, on: boolean): void {
  form.permissions = on ? form.permissions | bit : form.permissions & ~bit;
}

async function save(): Promise<void> {
  if (!editing.value && !/^[A-Za-z0-9_.-]{1,64}$/.test(form.name.trim())) {
    formError.value = "组名只能包含字母、数字与 _ - .，且不得超过 64 字节";
    return;
  }
  if (!form.displayName.trim()) {
    formError.value = "显示名不得为空";
    return;
  }
  // 域名的格式与重复校验都放在 addDomain 里逐行做过了，这里不再重复一遍。
  // 列表为空是合法状态（"这个组不托管任何域名"），不是错误。

  // 后端约定 -1 表示删除该项配额；界面上用"不限制"表达，不暴露这个魔法值。
  const quotas: Record<string, number> = {};
  for (const meta of QUOTA_META) {
    const draft = quotaDrafts[meta.key];
    if (!draft) {
      continue;
    }
    quotas[meta.key] = draft.enabled ? Math.max(0, Math.trunc(draft.value)) : -1;
  }

  saving.value = true;
  formError.value = "";
  try {
    await adminApi.saveGroup({
      name: form.name.trim(),
      displayName: form.displayName.trim(),
      permissions: form.permissions,
      priority: Math.trunc(form.priority),
      quotas,
      // 始终显式提交这个键，空列表也提交：[] 是"解绑全部域名"的明确意图，
      // 少传这个键后端会理解成"这次不动域名"，和管理员刚点下的操作对不上。
      // 后端对这三态（不传 / [] / 若干项）分别处理，不要试图用"空值"合并表达。
      receiveDomains: receiveDomains.value.map((row) => ({
        domain: row.domain,
        receiveEnabled: row.receiveEnabled,
      })),
    });
    toasts.success(editing.value ? "用户组已更新" : "用户组已创建");
    editorOpen.value = false;
    await load();
    // 管理员可能刚改的是自己所在的组：权限位变了，界面上的入口要跟着变。
    await refreshAfterAdminChange();
  } catch (err) {
    formError.value = describeError(err);
    logError("admin.groups.save", err);
  } finally {
    saving.value = false;
  }
}

// ---------------------------------------------------------------- 删除

const deleteTarget = ref<GroupDetail | null>(null);
const deleteBusy = ref(false);

async function confirmDelete(): Promise<void> {
  const target = deleteTarget.value;
  if (!target) {
    return;
  }
  deleteBusy.value = true;
  try {
    await adminApi.deleteGroup(target.group.name);
    toasts.success(`已删除用户组 ${target.group.name}`);
    deleteTarget.value = null;
    await load();
    await refreshAfterAdminChange();
  } catch (err) {
    toastApiError(toasts, err);
    logError("admin.groups.delete", err);
  } finally {
    deleteBusy.value = false;
  }
}

function editByName(name: string): void {
  const target = detailOf(name);
  if (target) {
    openEdit(target);
  }
}

function deleteByName(name: string): void {
  deleteTarget.value = detailOf(name) ?? null;
}
</script>

<template>
  <AdminPage :error="error">
    <Teleport v-if="topbarSlot" :to="topbarSlot">
      <AppButton size="sm" icon="refresh" :loading="loading" @click="load()">刷新</AppButton>
      <AppButton size="sm" variant="primary" icon="plus" @click="openCreate">新建用户组</AppButton>
    </Teleport>

    <div v-if="loading" class="card group-slot">
      <div class="group-loading">
        <span class="spinner" /> 正在加载
      </div>
    </div>
    <div v-else-if="rows.length === 0" class="card group-slot">
      <AppEmpty title="没有用户组" hint="预设组会在首次初始化时自动补齐。" />
    </div>
    <div v-else class="group-grid">
      <article v-for="row in rows" :key="row.name" class="card group-card">
        <header class="group-card__head">
          <div class="group-card__id">
            <span class="group-card__name">
              <span class="truncate">{{ row.displayName }}</span>
              <span v-if="row.isBuiltin" class="badge">预设</span>
              <span class="group-card__machine mono truncate">{{ row.name }}</span>
            </span>
          </div>
          <div class="actions">
            <AppButton
              size="sm"
              icon="edit"
              data-tip="编辑权限与配额"
              aria-label="编辑权限与配额"
              @click="editByName(row.name)"
            />
            <!-- 预设组不可删；后端同样会拒绝，前端直接不给入口。 -->
            <AppButton
              v-if="!row.isBuiltin"
              size="sm"
              variant="danger"
              icon="trash"
              :data-tip="row.memberCount > 0 ? '该组仍有成员' : '删除用户组'"
              aria-label="删除用户组"
              @click="deleteByName(row.name)"
            />
          </div>
        </header>

        <dl class="group-card__facts">
          <div class="group-card__fact">
            <dt>成员</dt>
            <dd class="mono">{{ row.memberCount }}</dd>
          </div>
          <div class="group-card__fact">
            <dt>权限</dt>
            <dd class="mono">{{ row.permCount }} 项</dd>
          </div>
          <div class="group-card__fact">
            <dt>优先级</dt>
            <dd class="mono">{{ row.priority }}</dd>
          </div>
        </dl>

        <!-- 一个组可以托管多个域名，逐个列出而不是只挑一个显示：
             收件开关按绑定生效，漏掉其中一条就等于把它的状态也漏掉了。 -->
        <div class="group-card__domain">
          <i class="ri-mail-line group-card__domain-icon" aria-hidden="true" />
          <template v-if="row.mailDomains.length > 0">
            <span
              v-for="item in row.mailDomains"
              :key="item.domain"
              class="group-card__domain-item"
              :class="{ 'is-off': !item.receiveEnabled }"
            >
              <span class="mono truncate" :title="item.domain">{{ item.domain }}</span>
              <!-- 每个域名有自己的开关：同一个域名可以被多个组共用，
                   各组的收件状态互不影响。 -->
              <span v-if="!item.receiveEnabled" class="badge badge--warn">已暂停</span>
            </span>
          </template>
          <!-- 没有域名与有域名但暂停是两回事：前者是"这个组不收信"，
               后者是"曾经收、现在先停"，恢复时地址与历史邮件都还在。 -->
          <span v-else class="faint">未设收件域名</span>
        </div>

        <div class="group-card__quotas">
          <ul v-if="row.quotaLines.length > 0" class="quota-lines">
            <li v-for="line in row.quotaLines" :key="line.label">
              <span class="truncate">{{ line.label }}</span>
              <strong class="mono">{{ line.value }}</strong>
            </li>
          </ul>
          <p v-else class="faint">全部不限</p>
        </div>
      </article>
    </div>

    <AppModal
      :open="editorOpen"
      wide
      :title="editing ? `编辑用户组 ${form.name}` : '新建用户组'"
      @close="editorOpen = false"
    >
      <div class="stack">
        <!-- 预设组的限制摆在表单最前面：先知道"不能改名"，再开始填。 -->
        <p v-if="editing && details.find((d) => d.group.name === form.name)?.group.isBuiltin" class="notice notice--info">
          预设组可修改权限与配额，但不可改名。
        </p>
        <div class="form-grid">
          <FormField
            label="组名"
            required
            hint="只允许字母、数字与 _ - .，创建后不可改名。"
          >
            <input
              v-model="form.name"
              class="input"
              :disabled="editing"
              placeholder="例如 vip"
            />
          </FormField>
          <FormField label="显示名" required>
            <input v-model="form.displayName" class="input" placeholder="例如 会员用户" />
          </FormField>
          <FormField label="优先级" hint="数值越大优先匹配；同名冲突时以它决定归属。">
            <input v-model.number="form.priority" class="input" type="number" step="1" />
          </FormField>
        </div>

        <!-- 收件域名放在组表单里而不是另开一页：它以「组 × 域名」的形式挂在组上，
             它本来就是组的属性。域名决定"寄到这个域的信落进谁的收件箱"。 -->
        <section class="group-receive">
          <h3 class="section__title">收件</h3>
          <div class="group-receive__body">
            <!-- 一行一个域名，开关跟着域名走而不是放在区块头部：
                 同一个域名可以被多个组共用，把开关放在区块级就分不清
                 暂停的是哪一个绑定，管理员会以为只停了自己这个组。 -->
            <div v-for="(row, index) in receiveDomains" :key="row.domain" class="group-domain">
              <div class="group-domain__row">
                <span class="group-domain__name mono truncate" :title="row.domain">{{ row.domain }}</span>
                <label class="check group-domain__toggle">
                  <input v-model="row.receiveEnabled" type="checkbox" />
                  <span class="check__text">接收新邮件</span>
                </label>
                <AppButton
                  size="sm"
                  variant="danger"
                  icon="trash"
                  class="group-domain__remove"
                  :disabled="row.addressCount > 0"
                  :data-tip="row.addressCount > 0 ? '域名下已有邮箱地址，不可移除' : '移除该域名'"
                  aria-label="移除收件域名"
                  @click="removeDomain(index)"
                />
              </div>
              <!-- 地址是用户数据：移除绑定等于让这些地址失去所属组，
                   换域名也要连带改写它们，代价远大于收益，因此只能先处理掉地址。 -->
              <p v-if="row.addressCount > 0" class="field__hint">
                该域名下已有 {{ row.addressCount }} 个邮箱地址，域名已锁定：解绑会让这些地址失去所属组，因此只能先处理掉这些地址。
              </p>
            </div>
            <p v-if="receiveDomains.length === 0" class="faint">未绑定收件域名，这个组收不到新邮件。</p>

            <FormField
              label="新增收件域名"
              hint="一个组可以托管多个域名，同一个域名也可以同时被多个组使用。本系统只收信，不代发。"
            >
              <div class="group-domain-add">
                <input
                  v-model="newDomain"
                  class="input"
                  placeholder="例如 vip.example.com"
                  @keyup.enter="addDomain"
                />
                <AppButton :disabled="!newDomain.trim()" @click="addDomain">添加</AppButton>
              </div>
            </FormField>
            <!-- 暂停的说明放在区块级而不是每行重复：它是"暂停"这个动作的
                 共性，逐行复制只会把域名名挤到放不下。收件开关按绑定生效，
                 这一句同时说明了"不会波及其它组"这个多对多的关键点。 -->
            <p class="field__hint">暂停只影响当前这个组，其它共用同一域名的组照常收信；重新打开即刻恢复，已有的邮箱地址和历史邮件不会丢。</p>
            <p class="field__hint">不添加任何域名 = 这个组不收信；保存时这份列表会整体替换原有的域名绑定。</p>
          </div>
        </section>

        <!-- 权限与配额并排：880px 宽弹窗里竖排会把窗口拉得很高，
             两栏后视口够宽时一眼能看全，窄视口自然折成单栏。 -->
        <div class="group-editor__cols">
          <section>
            <h3 class="section__title">权限</h3>
            <div class="perm-grid">
              <div v-for="group in permGroups" :key="group.title" class="card">
                <div class="card__body stack--tight">
                  <p class="perm-title">{{ group.title }}</p>
                  <label v-for="item in group.items" :key="item.bit" class="check">
                    <input
                      type="checkbox"
                      :checked="hasPerm(form.permissions, item.bit)"
                      @change="togglePerm(item.bit, ($event.target as HTMLInputElement).checked)"
                    />
                    <span class="check__text">{{ item.label }}</span>
                  </label>
                </div>
              </div>
            </div>
          </section>

          <section>
            <h3 class="section__title">配额</h3>
            <div class="stack--tight">
              <div v-for="meta in QUOTA_META" :key="meta.key" class="quota-row">
                <label class="check">
                  <input v-model="quotaDrafts[meta.key].enabled" type="checkbox" />
                  <span class="check__text">
                    <span>{{ meta.label }}</span>
                    <span class="faint">{{ meta.hint }}</span>
                  </span>
                </label>
                <!-- 勾上才出现输入框：五条灰掉的数字框排在下面只会制造噪音。
                     字节类带单位下拉（MB/GB），换算在保存时做，草稿里存字节。 -->
                <div v-if="quotaDrafts[meta.key].enabled" class="quota-value">
                  <input
                    class="input"
                    type="number"
                    min="0"
                    step="1"
                    :value="quotaAmount(meta)"
                    @input="setQuotaAmount(meta, $event)"
                  />
                  <AppSelect
                    v-if="meta.bytes"
                    v-model="quotaDrafts[meta.key].unit"
                    class="quota-unit"
                    aria-label="单位"
                    :options="UNIT_OPTIONS"
                  />
                  <span v-else class="faint nowrap">个会话</span>
                </div>
              </div>
            </div>
            <p class="field__hint">字节类配额按所选单位（MB/GB）填写；0 表示完全禁止，取消勾选表示不限制。</p>
          </section>
        </div>

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
      confirm-text="删除用户组"
      :loading="deleteBusy"
      :message="`删除用户组 ${deleteTarget?.group.name ?? ''}？`"
      detail="该组仍被邀请码引用、仍有成员、或仍绑着收件域名时后端会拒绝。删除后引用它的邀请码将无法再建立账号。"
      @confirm="confirmDelete"
      @cancel="deleteTarget = null"
    />
  </AdminPage>
</template>

<style scoped>
/* 加载与空态占的是"用户组卡片本来会在的那个格子"：套上和 .group-card 同一个
   .card 容器，页面才不会一半是卡片、一半悬着一段没有落点的文字。 */
.group-slot {
  width: 100%;
}

.group-loading {
  display: flex;
  align-items: center;
  justify-content: center;
  gap: var(--sp-2);
  padding: var(--sp-6);
  color: var(--c-text-muted);
}

/* 全局的 .actions 样式挂在 .table 下，卡片布局里要自己排（同 UsersView）。 */
.group-card .actions {
  display: flex;
  gap: var(--sp-2);
  flex: none;
}

.group-grid {
  display: grid;
  gap: var(--sp-4);
  grid-template-columns: repeat(auto-fill, minmax(min(300px, 100%), 1fr));
}

/* 配额清单长短不一，卡片拉到同高时限额块沉底，同排卡片的三段结构对得齐。 */
.group-card {
  display: flex;
  flex-direction: column;
}

.group-card__head {
  padding: var(--sp-4);
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: var(--sp-3);
}

.group-card__id {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.group-card__name {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  font-size: var(--fs-md);
  font-weight: 680;
  color: var(--c-text);
  min-width: 0;
}

.group-card__machine {
  font-size: var(--fs-xs);
  font-weight: 500;
  color: var(--c-text-faint);
}

.group-card__facts {
  display: grid;
  grid-template-columns: repeat(3, 1fr);
  margin: 0;
  border-top: 1px solid var(--c-border);
  border-bottom: 1px solid var(--c-border);
}

.group-card__fact {
  padding: var(--sp-2) var(--sp-4);
  display: flex;
  align-items: baseline;
  gap: var(--sp-2);
}

.group-card__fact + .group-card__fact {
  border-left: 1px solid var(--c-border);
}

.group-card__fact dt {
  font-size: var(--fs-xs);
  color: var(--c-text-faint);
}

.group-card__fact dd {
  margin: 0;
  font-size: var(--fs-sm);
  font-weight: 600;
  color: var(--c-text);
}

/* 收件域名独占一行：它决定这个组"能不能收到信"，
   埋在配额清单里会被当成一条普通限额扫过去。 */
.group-card__domain {
  margin: 0 var(--sp-4);
  padding: var(--sp-2) 0;
  display: flex;
  align-items: center;
  gap: var(--sp-1) var(--sp-2);
  /* 域名多了要能换行：窄卡片里硬挤成一行，每条都会被 truncate 成看不出区别。 */
  flex-wrap: wrap;
  min-width: 0;
  font-size: var(--fs-xs);
  color: var(--c-text);
  border-bottom: 1px dashed color-mix(in srgb, var(--c-border) 70%, transparent);
}

/* 一条域名 + 它的"已暂停"徽标。徽标跟着各自的域名走，
   整体右推（原先的 margin-left:auto）在多条时会把徽标甩到别人的域名旁边。 */
.group-card__domain-item {
  display: inline-flex;
  align-items: center;
  gap: var(--sp-1);
  min-width: 0;
}

/* 只把暂停的那一条压暗：整块压暗会让"还开着收件的那几条"也跟着看起来是停的。 */
.group-card__domain-item.is-off {
  color: var(--c-text-faint);
}

.group-card__domain-icon {
  font-size: 14px;
  color: var(--c-text-faint);
  flex: none;
}

.group-card__quotas {
  margin-top: auto;
  padding: var(--sp-3) var(--sp-4) var(--sp-4);
}

.quota-lines {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: 2px;
}

.quota-lines li {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  gap: var(--sp-3);
  min-width: 0;
  font-size: var(--fs-xs);
  color: var(--c-text-muted);
}

.quota-lines strong {
  font-weight: 600;
  color: var(--c-text);
  font-size: var(--fs-xs);
}

.perm-grid {
  display: grid;
  gap: var(--sp-3);
  grid-template-columns: repeat(auto-fit, minmax(min(220px, 100%), 1fr));
}

.perm-title {
  font-weight: 600;
  font-size: var(--fs-sm);
}

/* 收件区：域名行与新增输入左对齐成一块，窄视口下自然竖排。 */
.group-receive {
  padding: var(--sp-3) var(--sp-4);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
}

.group-receive__body {
  display: flex;
  flex-direction: column;
  gap: var(--sp-2);
}

/* 域名行和新增字段共用一条宽度上限：880px 宽的弹窗里铺满会让
   "域名 — 它的开关"这对关系拉得太开，看着不像一组。 */
.group-receive__body .field,
.group-domain__row {
  max-width: 520px;
}

.group-domain {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
}

/* 一行 = 域名 + 自己的开关 + 移除。开关与移除按内容宽靠右排成一列，
   域名独占剩下的宽度并可 truncate：长域名不会把按钮挤出弹窗。 */
.group-domain__row {
  display: flex;
  align-items: center;
  gap: var(--sp-3);
}

.group-domain__name {
  flex: 1;
  min-width: 0;
  font-size: var(--fs-sm);
  color: var(--c-text);
}

.group-domain__toggle,
.group-domain__remove {
  flex: none;
}

/* 新增输入的左边界与上面的域名行对齐，"添加"落在移除按钮同一列。 */
.group-domain-add {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
}

.group-domain-add .input {
  flex: 1;
  min-width: 0;
}

.group-editor__cols {
  display: grid;
  gap: var(--sp-5);
  grid-template-columns: minmax(0, 1fr);
  align-items: start;
}

@media (min-width: 720px) {
  .group-editor__cols {
    grid-template-columns: minmax(0, 3fr) minmax(0, 2fr);
  }
}

.quota-row {
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
}

/* 输入框缩进到标签文字正下方：与复选框对齐看起来像第二选项。 */
.quota-value {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding-left: 22px;
}

.quota-value .input {
  width: 120px;
}

/* 单位下拉只占内容宽：.asel 默认 width:100%，在 flex 行里会被拉满剩余宽度。 */
.quota-unit {
  flex: none;
  width: auto;
}
</style>
