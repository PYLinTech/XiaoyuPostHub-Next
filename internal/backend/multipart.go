package backend

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
)

const multipartRefPrefix = "xphv1:"

// ObjectPart 是逻辑对象在后端的一段连续字节区间。
type ObjectPart struct {
	Ref    string `json:"ref"`
	Offset int64  `json:"offset"`
	Size   int64  `json:"size"`
}

type multipartRef struct {
	Version int          `json:"v"`
	Parts   []ObjectPart `json:"parts"`
}

// ComposeObjectRef 把按逻辑偏移排列的物理对象合成为一个稳定定位符。
func ComposeObjectRef(parts []ObjectPart) (string, error) {
	if len(parts) == 0 {
		return "", fmt.Errorf("backend: 逻辑对象至少需要一个物理卷")
	}
	if len(parts) == 1 && parts[0].Offset == 0 && parts[0].Size > 0 {
		if parts[0].Ref == "" {
			return "", fmt.Errorf("backend: 逻辑对象物理卷清单无效")
		}
		return parts[0].Ref, nil
	}
	copyParts := append([]ObjectPart(nil), parts...)
	var offset int64
	for i := range copyParts {
		p := &copyParts[i]
		if p.Ref == "" || p.Offset != offset || p.Size <= 0 || offset > math.MaxInt64-p.Size {
			return "", fmt.Errorf("backend: 逻辑对象物理卷清单无效")
		}
		offset += p.Size
	}
	raw, err := json.Marshal(multipartRef{Version: 1, Parts: copyParts})
	if err != nil {
		return "", fmt.Errorf("backend: 编码物理卷清单失败: %w", err)
	}
	return multipartRefPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// SplitObjectRef 解析复合定位符；普通定位符作为单卷返回。
func SplitObjectRef(ref string, size int64) ([]ObjectPart, error) {
	if len(ref) < len(multipartRefPrefix) || ref[:len(multipartRefPrefix)] != multipartRefPrefix {
		if ref == "" || size < 0 {
			return nil, fmt.Errorf("backend: 对象定位符或长度无效")
		}
		return []ObjectPart{{Ref: ref, Offset: 0, Size: size}}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(ref[len(multipartRefPrefix):])
	if err != nil {
		return nil, fmt.Errorf("backend: 复合对象定位符编码无效")
	}
	var manifest multipartRef
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.Version != 1 || len(manifest.Parts) == 0 {
		return nil, fmt.Errorf("backend: 复合对象定位符清单无效")
	}
	var offset int64
	for _, p := range manifest.Parts {
		if p.Ref == "" || p.Offset != offset || p.Size <= 0 || offset > math.MaxInt64-p.Size {
			return nil, fmt.Errorf("backend: 复合对象定位符区间无效")
		}
		offset += p.Size
	}
	if size >= 0 && offset != size {
		return nil, fmt.Errorf("backend: 复合对象长度与清单不一致")
	}
	return manifest.Parts, nil
}
