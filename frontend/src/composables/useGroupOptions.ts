import { computed, ref, type ComputedRef, type Ref } from "vue";
import { adminApi } from "@/api/endpoints";
import type { Group } from "@/api/types";
import { describeError, logError } from "@/lib/async";

// 管理端「选用户组」这块数据的公共来源。
//
// 抽出来是因为它原本在 UsersView 与 InvitesView 里各抄了一份逐字相同的
// groupOptions / groupLabel / loadGroups（连注释都是同一句），而两份的错误处理
// 还不一致：一份把错误存进 groupsError 给弹窗提示，另一份只打了控制台。
// 两份副本意味着"组名怎么显示""读不到时退回什么"各改各的，迟早只改一处——
// 同一个下拉在两个页面写出两种格式，而这类不一致在界面上看不出来。
//
// 这里只提供「读列表 + 映射成选项/显示名」，不含任何页面专属判断：
// 读不到组列表时退回默认组 normal 是两页共同的底线，而"要不要在界面上明说"
// 留给调用方（UsersView 的改组需要专门权限，缺了必须提示；InvitesView 只是
// 建码时选一个组名，缺列表也能照常用后端默认的 normal）。

export interface GroupOption {
  value: string;
  label: string;
}

export interface GroupOptionsState {
  groups: Ref<Group[]>;
  /** 组列表读不到的原因；空串表示读到了。是否展示由调用方决定。 */
  groupsError: Ref<string>;
  /** 下拉选项：显示名 + 机器名，读不到列表时退回默认组。 */
  groupOptions: ComputedRef<GroupOption[]>;
  /** 显示名。库里的 group_name 是机器名（admin / normal），给人看的应是显示名。 */
  groupLabel: (name: string) => string;
  loadGroups: () => Promise<void>;
}

/**
 * @param scope 控制台日志前缀（如 "admin.users"），让组列表读失败时能分清是哪一页。
 */
export function useGroupOptions(scope: string): GroupOptionsState {
  const groups = ref<Group[]>([]);
  const groupsError = ref("");

  /** 组列表读不到时退回默认组，避免下拉里一个选项都没有。 */
  const groupOptions = computed<GroupOption[]>(() =>
    groups.value.length
      ? groups.value.map((group) => ({ value: group.name, label: `${group.displayName}（${group.name}）` }))
      : [{ value: "normal", label: "普通用户" }],
  );

  function groupLabel(name: string): string {
    return groups.value.find((item) => item.name === name)?.displayName ?? name;
  }

  async function loadGroups(): Promise<void> {
    try {
      const result = await adminApi.listGroups();
      groups.value = result.items.map((detail) => detail.group);
      groupsError.value = "";
    } catch (err) {
      // 组列表只决定"下拉里有哪些选项"，读不到不等于这页不可用：保留默认组即可。
      // 这里只记录不抛出，页面自己决定要不要把它显示出来。
      groupsError.value = describeError(err);
      logError(`${scope}.groups`, err);
    }
  }

  return { groups, groupsError, groupOptions, groupLabel, loadGroups };
}
