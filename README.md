# Enterprise IM

集团型多组织企业即时通信项目。产品基线见 [v2.2 需求文档](enterprise_im_group_v2_2.docx)，实施路径见 [开发计划](docs/企业IM-开发计划与实施路径-v0.1.md)。

## 当前开发增量

本分支实现 P1-01 集团模型与通信策略核心，并增加 P1-02a 管理授权、受限人员查询和单组织离职服务。公共 HTTP 入口仍只暴露健康探针。**尚无登录、业务管理接口、聊天界面或消息收发功能。**

## 本地运行

需要 Go 1.27 和 PostgreSQL 16。数据库应启用 `btree_gist` 扩展；首次迁移需要具备创建扩展和表的权限。可用现有 PostgreSQL 实例，也可使用隔离的本地测试容器。

本地没有 `psql` 时，可用 Docker 启动仅监听本机的开发数据库，并在容器内执行迁移：

```sh
docker run --rm -d --name enterprise-im-dev-db -e POSTGRES_PASSWORD=local_only_password -e POSTGRES_DB=enterprise_im -p 127.0.0.1:55432:5432 postgres:16-alpine
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000001_group_foundation.up.sql
docker exec -i enterprise-im-dev-db psql -U postgres -d enterprise_im -v ON_ERROR_STOP=1 < db/migrations/000002_admin_access.up.sql
```

迁移脚本包含显式事务；执行中途出错时，已创建的表会回滚。

然后启动服务：

```sh
export IM_DATABASE_URL='postgres://postgres:local_only_password@127.0.0.1:55432/enterprise_im?sslmode=disable'
go run ./cmd/im-api
```

服务默认监听 `:8080`，可用 `IM_HTTP_ADDR` 修改。探针为 `GET /health/live` 与 `GET /health/ready`；数据库不可用时 ready 返回 503。不要在共享环境使用示例密码，服务不会在日志中打印 DSN。

## 测试

```sh
go test ./...
IM_TEST_DATABASE_URL='postgres://postgres:local_only_password@127.0.0.1:55432/enterprise_im?sslmode=disable' go test ./... -count=1
go vet ./...
```

集成测试为每个用例创建独立 schema 并清理；未提供 `IM_TEST_DATABASE_URL` 时跳过 PostgreSQL 集成测试。回滚时按 `000002`、`000001` 的逆序执行 Down 脚本，只对可丢弃的开发或测试数据库执行回滚。
