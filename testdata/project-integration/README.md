# P4-31 本地联调夹具

工具版本与摘要由versions.lock和历史file-runtime锁共同校验；仅使用本期获批的同版本mc重建摘要。服务在本机Docker固定Unix套接字上创建，端口绑定127.0.0.1，使用本轮资源标签及登记。

扫描准备将真实CVD复制到本轮0700目录，sigtool验证三份签名；daily构建时间必须非未来且在24小时内。需要更新时freshclam只写本轮目录。正向与负向clamd各使用独立配置、socket和0400只读manifest，负向MaxFileSize为26214400；产品其他限制不变。启动后、每个扫描门禁之前重新核验原生进程、socket、配置、二进制及CVD摘要。

macOS Unix socket路径必须短于104字节，实际运行根使用/private/tmp中的新短英文目录。Chrome通过锁定Node/Playwright启动，私有HOME与临时profile仅作用于浏览器子进程，并登记真实PID、版本和退出。该检查只证明浏览器工具可启动；IM浏览器业务由后续门禁验收。

Task1–8完成，Task9评审修正及最终验证完成，独立[Draft PR #80](https://github.com/leileipei/Enterprise_IM/pull/80)已交付；最终固定源K的17门禁、完整仓库、race与清理均通过。统一入口如下。本机结果不构成生产部署或客户验收。失败尝试保留原始失败记录，清理恢复另存证据，不改写失败结果。

## 统一入口

```sh
python3 scripts/test-project-integration.py --source-commit <40位已提交SHA> --output-dir /private/tmp/p431-<新短英文标识>
```

入口要求新绝对目录，验证本机固定工具及镜像，按该SHA导出独立源码；未提交代码不进入本轮。每门禁最长60分钟，全轮含清理最长6小时；SIGINT/SIGTERM或到期停止新增门禁并清理已登记资源。收到失败仍保留明确失败与未执行状态，不复用其他提交的结果。

`verification.json`记录17项门禁、原始/普通/helper计数、独立清单、工具和日志摘要、处置及清理结论。分享日志在share目录，原始与分享文件均0600；已知凭据、token和私钥格式脱敏，含秘密原件删除，无法安全发布则记录失败。清理失败时保留私密恢复登记，不删除身份不明资源。

导入子门禁采用required-only，显式报告完整套件未执行。全仓库与race由统一父入口独立执行；只有两者通过、17项必需门禁通过且清理确认为true，才可算本阶段本地验收通过。浏览器工具预检与实际IM浏览器门禁分别记录。

最终验收源码为`cd6e4fc6adb025a1634e4952f9382a962b7b389e`。完整中文记录见`docs/P4-31-本机完整联调验收-20261008.md`，原评审、3项Important修正与2项Minor deferred见`docs/verification/p4-31-review-rulings.md`。真实业务Node/Chrome均登记实际生命周期；Linux9份安全产物、2个二进制摘要和实际限额收据由最新交付校验重新核对。


最终状态（2026-10-08）：J文档校验阻断由R006的NUL路径协议修正；92项编排回归与K同源完整复验通过。新签名库按原24小时规则检查，独立[Draft PR #80](https://github.com/leileipei/Enterprise_IM/pull/80)已交付。
