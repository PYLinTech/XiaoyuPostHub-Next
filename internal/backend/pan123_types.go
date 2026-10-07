package backend

import "encoding/json"

// 本文件集中定义 123 开放平台的响应结构。
//
// 官方接口的响应信封统一为 {code, message, data}，code == 0 表示成功；
// 另有 401（令牌失效）与 429（请求过频）需要按可重试处理。

// baseResp 是所有响应的公共部分。
type baseResp struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	TraceID string `json:"x-traceID"`
}

// accessTokenResp 是开发者凭证换取的令牌。
type accessTokenResp struct {
	baseResp
	Data struct {
		AccessToken string `json:"accessToken"`
		ExpiredAt   string `json:"expiredAt"`
	} `json:"data"`
}

// userInfoResp 用于配额展示与连通性自检。
type userInfoResp struct {
	baseResp
	Data struct {
		UID            int64 `json:"uid"`
		SpaceUsed      int64 `json:"spaceUsed"`
		SpacePermanent int64 `json:"spacePermanent"`
		SpaceTemp      int64 `json:"spaceTemp"`
		DirectTraffic  int64 `json:"directTraffic"`
	} `json:"data"`
}

// panFile 是文件列表中的一项。type == 1 表示目录。
type panFile struct {
	FileName     string `json:"filename"`
	Size         int64  `json:"size"`
	CreateAt     string `json:"createAt"`
	UpdateAt     string `json:"updateAt"`
	FileID       int64  `json:"fileId"`
	Type         int    `json:"type"`
	Etag         string `json:"etag"`
	S3KeyFlag    string `json:"s3KeyFlag"`
	ParentFileID int64  `json:"parentFileId"`
	Category     int    `json:"category"`
	Status       int    `json:"status"`
	Trashed      int    `json:"trashed"`
}

// listResp 是目录列表响应。lastFileId 为 -1 表示已到末尾。
type listResp struct {
	baseResp
	Data struct {
		LastFileID int64     `json:"lastFileId"`
		FileList   []panFile `json:"fileList"`
	} `json:"data"`
}

// mkdirResp 是创建目录响应。
type mkdirResp struct {
	baseResp
	Data struct {
		DirID    int64  `json:"dirID"`
		DirName  string `json:"dirName"`
		ParentID int64  `json:"parentID"`
	} `json:"data"`
}

// downloadInfoResp 返回自用下载流量通道的地址。
type downloadInfoResp struct {
	baseResp
	Data struct {
		DownloadURL string `json:"downloadUrl"`
	} `json:"data"`
}

// directLinkResp 返回直链流量通道的地址。
type directLinkResp struct {
	baseResp
	Data struct {
		URL string `json:"url"`
	} `json:"data"`
}

// directLinkSwitchResp 是对文件夹启用/关闭直链空间的响应。
type directLinkSwitchResp struct {
	baseResp
	Data struct {
		Filename string `json:"filename"`
	} `json:"data"`
}

// uploadCreateResp 是分片上传的创建响应。
//
// SliceSize 由 123 决定，必须校验它是加密块大小的整数倍。
// Reuse 为真表示命中 123 侧秒传，可直接拿到 FileID。
type uploadCreateResp struct {
	baseResp
	Data struct {
		FileID      int64    `json:"fileID"`
		PreuploadID string   `json:"preuploadID"`
		Reuse       bool     `json:"reuse"`
		SliceSize   int64    `json:"sliceSize"`
		Servers     []string `json:"servers"`
	} `json:"data"`
}

// uploadCompleteResp 是上传完成轮询响应。
type uploadCompleteResp struct {
	baseResp
	Data struct {
		Completed bool  `json:"completed"`
		FileID    int64 `json:"fileID"`
	} `json:"data"`
}

// jsonBody 便于构造请求体。
type jsonBody map[string]any

func marshalBody(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}
