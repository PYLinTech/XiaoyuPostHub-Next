package backend

import (
	"errors"
	"fmt"
	"net/url"
)

// URL errors contain auth_key and relay credentials; never copy URLs to logs.
func safeRelayError(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return fmt.Errorf("上游 %s 请求失败: %w", u.Op, safeRelayError(u.Err))
	}
	return err
}
