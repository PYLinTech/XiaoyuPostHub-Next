<script setup lang="ts">
import { computed, ref, watch } from "vue";
import { toastApiError } from "@/lib/async";
import AppButton from "@/components/ui/AppButton.vue";
import AppEmpty from "@/components/ui/AppEmpty.vue";
import AppModal from "@/components/ui/AppModal.vue";
import AppSelect from "@/components/ui/AppSelect.vue";
import { useMailShell } from "@/stores/mail";
import { useToasts } from "@/stores/toast";

// 「我的邮箱地址」弹层：入口在列表页头部，开合状态由邮件 store 持有，
// 因此列表 → 详情之间切换不会把它弄丢（弹层挂在邮件页内）。
//
// 壳走 AppModal：Esc 关闭、滚动锁、打开移焦与焦点还原本来都是自己漏掉的，
// 而这些漏项在每个调用点都会各自重犯一遍。
const mail = useMailShell();
const toasts = useToasts();

const newLocalPart = ref("");
const addrBusy = ref(false);
const selectedDomain = ref("");

/** 正在申请解绑的地址；空串表示申请弹窗没开。 */
const unbindTarget = ref("");
const unbindReason = ref("");
/** 正在提交/撤销的地址，用来只锁住被点的那一行。 */
const unbindBusy = ref("");

// 域名不再从已有地址回填：它由管理员按组配置，弹层打开时从
// /api/mail/domains 取，用户侧只读。回填会让"我原来在哪个域下建过地址"
// 变成域名的来源，管理员改了绑定之后界面上还留着旧值。
/** 本组当前可用的域名。域名与组是多对多，这里可能有多项。 */
const domainOptions = computed(() =>
  mail.state.domains.map((item) => ({ value: item.domain, label: item.domain })),
);

/** 每次打开弹层都会重取域名列表，管理员可能已经改过绑定：选择掉出列表时
    退回第一项，而不是把一个后端不认的域名带进提交。 */
watch(domainOptions, (options) => {
  if (options.some((option) => option.value === selectedDomain.value)) {
    return;
  }
  selectedDomain.value = options[0]?.value ?? "";
});

/** 打开解绑申请弹窗。重开时清掉上次的理由。 */
function openUnbind(address: string): void {
  unbindTarget.value = address;
  unbindReason.value = "";
}

/**
 * 提交解绑申请。
 *
 * 成功后不关外层弹层：用户还要看到"这个地址现在待审核"，关掉等于要重新打开
 * 才能确认刚才那一下生效了。
 */
async function submitUnbind(): Promise<void> {
  const target = unbindTarget.value;
  if (!target) return;
  unbindBusy.value = target;
  try {
    await mail.requestUnbind(target, unbindReason.value.trim());
    unbindTarget.value = "";
    toasts.success("解绑申请已提交，等待管理员审核");
  } catch (err) {
    toastApiError(toasts, err);
  } finally {
    unbindBusy.value = "";
  }
}

/** 撤销自己的待审申请。撤销后可再次申请。 */
async function cancelUnbind(address: string): Promise<void> {
  unbindBusy.value = address;
  try {
    await mail.cancelUnbind(address);
    toasts.success("已撤销解绑申请");
  } catch (err) {
    toastApiError(toasts, err);
  } finally {
    unbindBusy.value = "";
  }
}

async function createAddress(): Promise<void> {
  const local = newLocalPart.value.trim();
  if (!local || !selectedDomain.value) {
    return;
  }
  addrBusy.value = true;
  try {
    await mail.createAddress(local, selectedDomain.value);
    newLocalPart.value = "";
    toasts.success("邮箱地址已创建");
  } catch (err) {
    toastApiError(toasts, err);
  } finally {
    addrBusy.value = false;
  }
}
</script>

<template>
  <AppModal :open="mail.state.addrOpen" title="我的邮箱地址" @close="mail.closeAddresses()">
    <div class="stack">
      <!-- addressesLoaded 之前是"还在取"，取回来是空才是"确实没有"。
           两者共用一个空数组，不分开就会在加载途中断言管理员没分配域名；
           取数失败由 store 关掉弹层并弹提示，这里不会走到失败那一态。 -->
      <ul v-if="mail.state.addresses.length" class="mv-addr-list">
        <li v-for="a in mail.state.addresses" :key="a.address">
          <i class="ri-at-line" />
          <span class="truncate">{{ a.address }}</span>
          <span class="badge" :class="a.status === 'active' ? 'ok' : ''">{{
            a.status === "active" ? "可用" : "已冻结"
          }}</span>
          <!-- 冻结地址不给申请：它本来就不收信，删不删没有区别，
               让用户在这种状态下提交只会制造一张没有标的的单子。 -->
          <template v-if="a.status === 'active'">
            <!-- 已有待审单就显示状态并允许撤销，而不是再放一个「申请」按钮：
                 后端会拒绝重复申请，界面上给一个点了必失败的按钮更糟。 -->
            <template v-if="mail.state.unbinds[a.address]">
              <span class="badge warn">待审核</span>
              <button class="link-btn" :disabled="unbindBusy === a.address" @click="cancelUnbind(a.address)">
                撤销
              </button>
            </template>
            <button v-else class="link-btn" @click="openUnbind(a.address)">申请解绑</button>
          </template>
        </li>
      </ul>
      <div v-else-if="!mail.state.addressesLoaded" class="table-state">
        <span class="spinner" /> 正在加载
      </div>
      <AppEmpty v-else title="还没有邮箱地址" />

      <!-- 域名是只读的：它由管理员在「设置 → 用户组」里按组配置，
           用户侧只能在已分配的那几个里选，不提供任意输入或改写。 -->
      <div v-if="selectedDomain" class="mv-addr-new">
        <input v-model="newLocalPart" type="text" placeholder="用户名" />
        <span class="mv-addr-at">@</span>
        <!-- 只有一项时直接写出来：为一个不会变的值摆一个下拉，
             只是让用户多点一次，还把"我在哪个域下建地址"藏了起来。 -->
        <span v-if="domainOptions.length < 2" class="mv-addr-domain mono">{{ selectedDomain }}</span>
        <AppSelect
          v-else
          v-model="selectedDomain"
          class="mv-addr-pick"
          aria-label="邮箱域名"
          :options="domainOptions"
        />
        <AppButton size="sm" :loading="addrBusy" :disabled="!newLocalPart.trim()" @click="createAddress">
          创建
        </AppButton>
      </div>
      <p v-else-if="mail.state.addressesLoaded" class="mv-addr-tip">
        系统管理员没有为当前用户组分配邮箱域名
      </p>
      <p v-else class="mv-addr-tip">正在读取本组可用的邮箱域名…</p>
      <p v-if="selectedDomain" class="mv-addr-tip">
        域名由管理员按用户组分配，只能在已分配的域名中选择。
      </p>

      <!-- 申请解绑：申请不等于删除。通过后地址立刻失效，发往它的邮件会被退信，
           已经收到的邮件全部保留——归属记在账号上，不在地址上。 -->
      <AppModal
        v-if="unbindTarget"
        :open="unbindTarget !== ''"
        title="申请解绑邮箱地址"
        @close="unbindTarget = ''"
      >
        <div class="stack">
          <p class="mv-addr-tip">
            即将申请解绑 <strong>{{ unbindTarget }}</strong>
          </p>
          <p class="mv-addr-tip">
            管理员审核通过后该地址会被删除，发往它的邮件将收到退信；你已经收到的邮件会全部保留。
          </p>
          <label class="mv-addr-tip">
            申请理由（选填）
            <input v-model="unbindReason" type="text" maxlength="200" placeholder="例如：不再使用这个地址" />
          </label>
          <AppButton size="sm" :loading="unbindBusy === unbindTarget" @click="submitUnbind">
            提交申请
          </AppButton>
        </div>
      </AppModal>
    </div>
  </AppModal>
</template>

<style scoped>
.mv-addr-list {
  list-style: none;
  margin: 0;
  padding: 0;
  display: flex;
  flex-direction: column;
  gap: var(--sp-1);
}

.mv-addr-list li {
  display: flex;
  align-items: center;
  gap: var(--sp-2);
  padding: var(--sp-2) var(--sp-3);
  border: 1px solid var(--c-border);
  border-radius: var(--r-md);
  background: var(--c-surface-sunken);
  font-size: var(--fs-sm);
}

.mv-addr-list .badge {
  margin-left: auto;
  font-size: 10px;
  color: var(--c-text-faint);
}

.mv-addr-list .badge.ok {
  color: var(--c-success);
}

.mv-addr-new {
  display: flex;
  align-items: center;
  gap: var(--sp-1);
}

.mv-addr-new input {
  flex: 1;
  min-width: 0;
  height: 32px;
  padding: 0 var(--sp-2);
  border: 1px solid var(--c-border);
  border-radius: var(--r-sm);
  background: var(--c-surface);
  color: var(--c-text);
  font-size: var(--fs-xs);
}

.mv-addr-new input:focus {
  outline: none;
  border-color: var(--c-accent);
}

.mv-addr-at {
  color: var(--c-text-faint);
}

/* 域名是只读展示，不是输入框：做成和输入框同样的高度与底色，
   读起来才像"这一段是固定的"，而不是一个忘了禁止编辑的框。
   32px 与上面 .mv-addr-new input 的高度写死成同一个值（tokens.css 里
   没有 --control-h，引用它只会整条失效、悄悄退回内容高度）。 */
.mv-addr-domain {
  display: inline-flex;
  align-items: center;
  min-height: 32px;
  padding: 0 var(--sp-3);
  border: 1px dashed var(--c-border-strong);
  border-radius: var(--r-md);
  background: var(--c-surface-sunken);
  color: var(--c-text-muted);
  font-size: var(--fs-sm);
  white-space: nowrap;
}

/* 多域名时的下拉要占上面那块的同一个位置：同样按内容收宽、同样收在 32px。
   .asel 默认 width:100%，不写会被拉满剩余宽度；:deep 是因为 .asel__trigger
   是 AppSelect 内部的类，scoped 选不到；上下内边距清零才不会被
   .select 的 8px 撑到 36px。 */
.mv-addr-pick {
  flex: none;
  width: auto;
}

.mv-addr-pick :deep(.asel__trigger) {
  min-height: 32px;
  padding-top: 0;
  padding-bottom: 0;
  font-size: var(--fs-sm);
}

.mv-addr-tip {
  margin: 0;
  font-size: 10px;
  color: var(--c-text-faint);
  line-height: 1.5;
}
</style>
