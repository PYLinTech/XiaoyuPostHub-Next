package httpapi

import (
	"net/http"
	"strconv"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/auth"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/store"
)

type sessionPayload struct {
	Token       string      `json:"token"`
	User        store.User  `json:"user"`
	Group       store.Group `json:"group"`
	Permissions int64       `json:"permissions"`
	ExpiresAt   int64       `json:"expiresAt"`
}

type loginRequest struct {
	Account  string `json:"account"`
	Password string `json:"password"`
}

// handleLogin 登录并签发会话。
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := s.Auth.Login(r.Context(), auth.LoginRequest{
		Account:   req.Account,
		Password:  req.Password,
		ClientIP:  clientIPOf(r.Context()),
		UserAgent: r.UserAgent(),
	})
	if err != nil {
		if auth.IsThrottled(err) {
			// 把建议的等待时长告诉客户端，避免它立刻重试把退避期不断延长。
			if retry := auth.RetryAfter(err); retry > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())))
			}
		}
		fail(w, err)
		return
	}
	writeData(w, sessionPayload{
		Token:       result.Token,
		User:        result.Principal.User,
		Group:       result.Principal.Group,
		Permissions: result.Principal.Permissions(),
		ExpiresAt:   result.Principal.Session.ExpiresAt.Unix(),
	})
}

// handleLogout 吊销当前会话。
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.Auth.Logout(r.Context(), tokenOf(r.Context())); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}

type registerRequest struct {
	Account    string `json:"account"`
	Password   string `json:"password"`
	InviteCode string `json:"inviteCode"`
}

// handleRegister 按注册模式创建账号。
func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	user, err := s.Svc.Register(r.Context(), service.RegisterRequest{
		Account:    req.Account,
		Password:   req.Password,
		InviteCode: req.InviteCode,
		ClientIP:   clientIPOf(r.Context()),
	})
	if err != nil {
		fail(w, err)
		return
	}
	// 只回传公开字段：store.User 的敏感字段已用 json:"-" 屏蔽。
	writeData(w, map[string]any{"user": user})
}

// handleProfile 返回当前身份的资料与配额使用情况。访客也可调用，
// 用于前端决定界面上显示哪些入口。
func (s *Server) handleProfile(w http.ResponseWriter, r *http.Request) {
	profile, err := s.Svc.Profile(r.Context(), principalOf(r.Context()))
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, profile)
}

type changePasswordRequest struct {
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

// handleChangePassword 修改自己的口令，并吊销其它设备上的会话。
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	p, ok := s.requireUser(w, r)
	if !ok {
		return
	}
	var req changePasswordRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.Svc.ChangeOwnPassword(r.Context(), p, req.OldPassword, req.NewPassword); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}
