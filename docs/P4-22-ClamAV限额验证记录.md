# P4-22 扫描限额验证记录

验证日期：2026-10-04。对象：专用本地 ClamAV 1.5.4、qpdf 12.4.2 和真实病毒定义库。此记录不是生产上线验收。

## 实测问题

原设计设置上传和 INSTREAM 上限为 26,214,400 字节、MaxFileSize 为 26,214,400 字节、MaxScanSize 为 262,144,000 字节，同时打开超限、加密及损坏报警。

使用合法嵌套 ZIP 对照：内层 ZIP 为 26,280,218 字节，外层压缩 ZIP 为 25,928 字节；内层先放填充条目，再在最后放标准 68 字节 EICAR 测试条目。

| 专用进程配置 | 实际响应 |
| --- | --- |
| 原 MaxFileSize=26,214,400 | stream: OK |
| 候选 MaxFileSize=0、全局展开配额仍为250MiB，相关内部解析限额提高至全局配额 | stream: Eicar-Test-Signature FOUND |

候选进程对展开超过250MiB的压缩样本返回超限报警。它仅用于对照试验，尚未作为产品配置采用。将 EICAR 追加到大文本末尾不是可靠的标准病毒检测对照，本结论仅使用独立标准条目的嵌套 ZIP 证据。

上游相关问题：[压缩子文件超限后未报警](https://github.com/Cisco-Talos/clamav/issues/633)、[PCRE 限额跳过](https://github.com/Cisco-Talos/clamav/issues/1785)。协议依据：[Clamd Protocol](https://docs.clamav.net/manual/Usage/ClamdProtocol.html)。

## 当前实现与验证状态

- 扫描默认不可用；原配置无法通过启动行为预检，不能输出 ready。
- 运行时绑定实际 PID、启动参数、Unix socket、只读 manifest/config、固定二进制 SHA、CVD 指纹与库版本。定义库须早于进程启动且距构建时间不超过24小时；每次扫描前后重新核实。
- 本地进程模式使用实际二进制指纹；不能将该证据视为生产容器镜像验收。受限 TCP 端点不受支持。
- 真实 qpdf 完整一页 PDF 通过；加密 PDF 拒绝；损坏、异常退出和超时失败。零页 qpdf --empty 文件不是合法通过样本。
- TXT 完整 UTF-8/NUL、MIME/扩展一致、PNG/JPEG 完整解码及像素预算检查已验证。
- 专用 Linux 512MiB/1CPU 容器内，40,000,000 像素16位RGBA PNG完整解码通过；40,000,001 像素在解码前拒绝，边界样本仅用来验证尺寸预算。
- `run-scan` 包含必须能输出实际 ready 的正向验收；不能用“失败关闭测试通过”替代组件可用验收。

## 待决策

已申请确认：上传和 INSTREAM 仍限25MiB，取消会静默跳过的单子文件限额，使用250MiB全局展开配额并锁定相关内部解析限额，重新执行全部真实限额验收。在确认和正向验收完成前，不启用文件扫描功能。
