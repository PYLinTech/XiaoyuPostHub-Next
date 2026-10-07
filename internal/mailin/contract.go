// Package mailin 是入站 SMTP 协议层（smtpd）与业务编排层（service）之间
// 的契约包。它不依赖任何内部包：协议侧只认这里的接口与错误形态，业务侧
// 只在这里表达"临时/永久拒收"，避免两个方向上的包循环。
package mailin

import (
	"context"
	"errors"
	"io"
)

// Envelope 是一次入站投递在 DATA 之前收集到的信封信息。
type Envelope struct {
	// RemoteIP 是对端 IP 字面量（SPF 与审计用）。
	RemoteIP string
	// Helo 是客户端 EHLO/HELO 自报名称。
	Helo string
	// MailFrom 是信封发件人（MAIL FROM），空串表示空反向路径（退信）。
	MailFrom string
	// Recipients 是通过校验并去重后的信封收件人（RCPT TO）。
	Recipients []string
	// SPFResult 是本连接的 SPF 结果词（小写）；策略关闭时为空。
	SPFResult string
}

// Receiver 由业务编排层实现，供协议服务器在 SMTP 各阶段回调。
//
// 拒收一律返回 *StatusError 携带 4xx/5xx；返回普通 error 时协议层按
// 451 临时失败处理（对方稍后重试是安全的）。
type Receiver interface {
	// VerifyRecipient 校验单个 RCPT。declaredSize 来自 MAIL FROM 的
	// ESMTP SIZE 参数（客户端未声明时为 0），用于按收件人剩余邮件
	// 存储配额提前返 452，避免收完整封信才告诉对方装不下。
	VerifyRecipient(ctx context.Context, address string, declaredSize int64) error

	// CheckSender 在 MAIL FROM 后执行发件策略（SPF）。返回值是落库用的
	// 结果词（pass/fail/softfail/neutral/none/temperror/permerror），
	// 策略关闭时返回空串。硬失败是否拒收由实现按站点策略决定。
	CheckSender(ctx context.Context, remoteIP, helo, mailFrom string) (string, error)

	// Receive 消费完整 DATA（已做点消除的原始 RFC822 字节）并完成落信。
	// 必须按 RFC Message-ID 幂等：对方重试同一封邮件不得产生两封。
	Receive(ctx context.Context, env Envelope, raw io.Reader) error
}

// StatusError 是带 SMTP 状态码的显式拒收。
type StatusError struct {
	Code    int
	Message string
}

func (e *StatusError) Error() string { return e.Message }

// Reject 构造一个带状态码的拒收。
func Reject(code int, msg string) *StatusError {
	return &StatusError{Code: code, Message: msg}
}

// AsStatusError 从错误链中取出 *StatusError；没有则返回 nil。
// errors.As 自身会遍历 errors.Join 形成的多分支链——落信路径会把
// "回退配额失败"等级联错误 join 到主拒收上，协议层仍需看到主状态码。
func AsStatusError(err error) *StatusError {
	if err == nil {
		return nil
	}
	var se *StatusError
	if errors.As(err, &se) {
		return se
	}
	return nil
}
