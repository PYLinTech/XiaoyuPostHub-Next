// 账号与密码的长度规则。
//
// 这两个值是固定约束，不在站点配置里可调（后端 `internal/service/accounts.go`
// 的 AccountMinChars / AccountMaxChars 与 `internal/auth/password.go` 的
// MinPasswordChars / MaxPasswordChars 是同一份规则的权威定义）。前端放在这里
// 只为了"提交前先给出可读的提示"，真正的判定仍在服务端。
//
// 密码只接受可打印 ASCII 字符：大小写英文、数字和符号，不含空格或其它 Unicode。

export const ACCOUNT_MIN = 3;
export const ACCOUNT_MAX = 16;
export const PASSWORD_MIN = 8;
export const PASSWORD_MAX = 32;

export const ACCOUNT_PLACEHOLDER = `请输入${ACCOUNT_MIN}-${ACCOUNT_MAX}位中英文或符号`;
export const PASSWORD_PLACEHOLDER = `请输入${PASSWORD_MIN}-${PASSWORD_MAX}位大小写英文字母、数字或符号`;

/** 按字符（code point）计数，避免把 emoji、代理对算成两个。 */
function charLength(value: string): number {
  return Array.from(value).length;
}

/** 校验账号；通过返回空串，否则返回可直接展示的原因。 */
export function validateAccount(account: string): string {
  const value = account.trim();
  if (value.length === 0) {
    return "请输入账号";
  }
  const length = charLength(value);
  if (length < ACCOUNT_MIN || length > ACCOUNT_MAX) {
    return `账号需要 ${ACCOUNT_MIN}-${ACCOUNT_MAX} 位中英文或符号`;
  }
  if (/\s/.test(value)) {
    return "账号不能包含空格";
  }
  return "";
}

/** 校验密码；通过返回空串，否则返回可直接展示的原因。 */
export function validatePassword(password: string): string {
  if (password.length === 0) {
    return "请输入密码";
  }
  if (!/^[\x21-\x7e]+$/.test(password)) {
    return "密码只能使用大小写英文字母、数字和符号，不能包含空格";
  }
  if (password.length < PASSWORD_MIN || password.length > PASSWORD_MAX) {
    return `密码需要 ${PASSWORD_MIN}-${PASSWORD_MAX} 位大小写英文字母、数字或符号`;
  }
  return "";
}
