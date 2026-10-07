package httpapi

import (
	"net/http"

	"github.com/PYLinTech/XiaoyuPostHub-Next/internal/service"
)

// 首次初始化入口。
//
// 这组接口在初始化完成前是**公开**的——否则运维没有任何途径创建第一个管理员。
// 公开意味着它必须自己解决"谁有资格调用"：所有来源都必须带上启动日志里打印
// 的一次性 token，本机回环也不例外。初始化完成后 POST /api/setup/init 立即
// 返回 409，整套入口自动失效。

// handleSetupState 返回站点初始化状态。
//
// 它是公开接口：前端要在登录之前就知道该显示引导页还是登录页，因此只返回引导页
// 必需的信息，未初始化时附带落盘路径供运维确认。
func (s *Server) handleSetupState(w http.ResponseWriter, r *http.Request) {
	state, err := s.Svc.SetupState(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, state)
}

// handleSetupInit 执行一次性初始化。
func (s *Server) handleSetupInit(w http.ResponseWriter, r *http.Request) {
	var req service.SetupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	result, err := s.Svc.CompleteSetup(r.Context(), req)
	if err != nil {
		fail(w, err)
		return
	}
	writeData(w, result)
}

// handleSetupValidate 校验引导页的单步输入。
//
// 引导页点"下一步"时就带上这一步的字段过来，校验通过才前进。令牌填错、
// 账号不合规、密钥格式不对都在当场暴露，而不是让人填完整套流程到最后
// 提交时才知道。
//
// 它和 init 一样是公开接口，但只做无副作用的读取：token 步骤会比对一次性
// 令牌，其余步骤只跑与提交时完全相同的纯校验函数，不写库、不改配置、不对
// 存储发起连通性探测。已初始化的实例一律返回 409。
func (s *Server) handleSetupValidate(w http.ResponseWriter, r *http.Request) {
	var req service.SetupStepValidateRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.Svc.ValidateSetupStep(r.Context(), req); err != nil {
		fail(w, err)
		return
	}
	writeData(w, map[string]any{"ok": true})
}
