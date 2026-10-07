# P4-31 本地联调夹具

工具版本与摘要由versions.lock和历史file-runtime锁共同校验；仅使用本期获批的同版本mc重建摘要。服务在本机Docker固定Unix套接字上创建，端口绑定127.0.0.1，使用本轮资源标签及登记。

扫描准备将真实CVD复制到本轮0700目录，sigtool验证三份签名；daily构建时间必须非未来且在24小时内。需要更新时freshclam只写本轮目录。正向与负向clamd各使用独立配置、socket和0400只读manifest，负向MaxFileSize为26214400；产品其他限制不变。启动后、每个扫描门禁之前重新核验原生进程、socket、配置、二进制及CVD摘要。

macOS Unix socket路径必须短于104字节，实际运行根使用/private/tmp中的新短英文目录。Chrome通过锁定Node/Playwright启动，私有HOME与临时profile仅作用于浏览器子进程，并登记真实PID、版本和退出。该检查只证明浏览器工具可启动；IM浏览器业务由后续门禁验收。

Task1–5已完成阶段验证，统一入口尚未实现。阶段绿色不代表17门禁、完整仓库、race、生产部署或客户验收完成。失败尝试保留原始失败记录，清理恢复另存证据，不改写失败结果。
