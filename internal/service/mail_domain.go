// 收件域名由管理员在用户组表单里分配，域名本身不是独立管理对象。
//
// 组与域名是多对多：一个组可托管多个域名，同一个域名也可同时托管给多个组
// （按部门分组共用一个企业域名是常见需求）。每个「组 × 域名」绑定自带收件
// 开关，互不影响——开关放在域名上的话，在 A 组里暂停会静默改掉共用同一域名
// 的 B 组。
//
// 因此本文件里没有"改名"这回事：域名的身份就是那串文本，而 mail_addresses
// 与历史邮件都按文本外键留存。管理员想换域名就是新增一个 + 移除旧的，集合
// 运算的结果与改名完全一致，却不需要改写任何既有引用。
//
// 本系统只收信，域名不承载任何发信凭据。
package service

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

// domainRe 宽松校验域名：点分标签、字母数字连字符、总长 ≤253。
var domainRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+$`)

// normalizeDomain 统一小写并去空白。域名在 DNS 与外键里都是小写，
// 大小写混着存会让同一个域名出现两行。
func normalizeDomain(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func validDomain(domain string) bool {
	if len(domain) == 0 || len(domain) > 253 {
		return false
	}
	return domainRe.MatchString(domain)
}

// normalizedDomains 归一化一个域名列表，供审计记录使用：把"管理员提交了什么"
// 变成"库里实际会是什么"，免得审计里留下大小写混杂的原文。
func normalizedDomains(list []GroupMailDomainInput) []string {
	out := make([]string, 0, len(list))
	for _, item := range list {
		out = append(out, normalizeDomain(item.Domain))
	}
	return out
}

// syncGroupMailDomains 把用户组表单里的收件域名列表同步到库中。
//
// want 是**完整期望集合**而非增量：先解绑本次没出现的旧绑定，再绑定新的。
// 按集合同步而不是逐条增删，是因为管理员心里的模型就是"这个组就该有这几个
// 域名"——从表单移掉一行就意味着不再托管，集合同步让这个意图不需要额外的
// "应用"动作，也不会漏删。
//
// 调用方必须已经建好或更新了组行：绑定的外键指向 user_groups(name)，同一
// 事务里先有组行才写得进去。
func syncGroupMailDomains(ctx context.Context, tx store.Querier, groupName string, want []GroupMailDomainInput) error {
	current, err := store.ListGroupMailDomains(ctx, tx, groupName)
	if err != nil {
		return err
	}

	kept := make(map[string]bool, len(want))
	normalized := make(map[string]bool, len(want))
	for _, item := range want {
		domain := normalizeDomain(item.Domain)
		if domain == "" {
			return fmt.Errorf("%w: 收件域名不得为空", ErrBadRequest)
		}
		if !validDomain(domain) {
			return fmt.Errorf("%w: 收件域名 %s 格式不正确", ErrBadRequest, domain)
		}
		if normalized[domain] {
			// 表单里同一个域名出现两次不是错误，但它会让"集合"的含义变得
			// 依赖遍历顺序。与其在这里默默合并，不如明确拒绝：界面上通常
			// 是复制粘贴时出的错，早说比晚说好。
			return fmt.Errorf("%w: 收件域名 %s 重复", ErrBadRequest, domain)
		}
		normalized[domain] = true
		kept[domain] = true
	}

	// 移除本次不再托管的域名。域名下面还有本组用户的邮箱地址时挡下：那些
	// 地址是用户数据，一旦随解绑失去所属组就会变成永远收不到信的孤儿，
	// 而管理员要做的只是先把地址处理掉。这比悄悄让一批地址失效诚实。
	for _, b := range current {
		if kept[b.Domain] {
			continue
		}
		n, err := store.CountGroupMailAddresses(ctx, tx, groupName, b.Domain)
		if err != nil {
			return err
		}
		if n > 0 {
			return fmt.Errorf("%w: 域名 %s 下仍有本组用户的 %d 个邮箱地址，无法移除；请先处理这些地址",
				ErrConflict, b.Domain, n)
		}
		if err := store.UnbindGroupMailDomain(ctx, tx, groupName, b.Domain); err != nil {
			return err
		}
		if err := dropDomainIfUnused(ctx, tx, b.Domain); err != nil {
			return err
		}
	}

	for _, item := range want {
		domain := normalizeDomain(item.Domain)
		if err := store.EnsureMailDomain(ctx, tx, domain); err != nil {
			return err
		}
		// 开关就是这次提交的值：它属于这条绑定，管理员在表单上勾的就是
		// 意图。既有的绑定同样按提交值写回——同步的是整份列表，不存在
		// "保留原值"这种隐含第三种语义。
		if err := store.BindGroupMailDomain(ctx, tx, groupName, domain, item.ReceiveEnabled); err != nil {
			return err
		}
	}
	return nil
}

// dropDomainIfUnused 在域名不再被任何组绑定、也没有邮箱地址时把它删掉。
//
// 域名行一旦没有任何组引用就只是残留：留着它会让"这个域名存在"与"这个域名
// 可用"变成两件事，而管理员能改的只有后者。
func dropDomainIfUnused(ctx context.Context, tx store.Querier, domain string) error {
	inUse, err := store.CountDomainsInUse(ctx, tx, domain)
	if err != nil {
		return err
	}
	if inUse > 0 {
		return nil
	}
	addrCount, err := store.CountMailAddressesAll(ctx, tx, domain)
	if err != nil {
		return err
	}
	if addrCount > 0 {
		// 地址还挂在这个域名上（多半是历史数据或已解绑组的遗留）。域名行
		// 是它们的外键目标，删不得，但也不再是任何组可绑的域名。
		return nil
	}
	if err := store.DeleteMailDomain(ctx, tx, domain); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	return nil
}
