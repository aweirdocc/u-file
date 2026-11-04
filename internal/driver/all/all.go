// Package all 统一导入全部驱动包，触发其 init() 注册到 driver 注册表。
// 新增驱动（如 ftp/sftp/dbblob/webdav）时，必须在此添加空导入。
package all

import (
	_ "ufile/internal/driver/alipan"
	_ "ufile/internal/driver/ftp"
	_ "ufile/internal/driver/gdrive"
	_ "ufile/internal/driver/local"
	_ "ufile/internal/driver/s3"
	_ "ufile/internal/driver/sftpdrv"
	_ "ufile/internal/driver/webdav"
)
