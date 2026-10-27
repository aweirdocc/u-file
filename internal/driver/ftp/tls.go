package ftp

import (
	"crypto/tls"
	"time"
)

// implicitTLSConfig 显式 TLS（AUTH TLS，FTPS 标准方式）。
func implicitTLSConfig(insecure bool) *tls.Config {
	return &tls.Config{
		InsecureSkipVerify: insecure, // FTPS 自签证书场景由用户显式选择
		MinVersion:         tls.VersionTLS12,
	}
}

var _ = time.Second // 保持 time 引用（DialWithTimeout 用到）
