# 新增 Domain Service Checklist

> **先用生成器**：`make new-domain NAME=<name>`（`scripts/new-domain.sh`）會自動完成
> 下方第 3 節（三層目錄 + migration + service 測試）與第 5 節（wire/router 接線，
> 再跑 `make wire`）。生成後**仍需手動**：第 2 節的 compose init 掛載與 e2e migration
> 列表、第 4 節的 rbac/opa 規則（生成的 `<name>:list|read|create|update|delete` actions
> 在 `AUTHZ_MODE=allow` 下即可用）、第 1/6 節文件與 BFF。
> 生成器本身由 CI 的 `new-domain-smoke` job（`scripts/new-domain-smoke.sh`）持續驗證。
> 完整試驗（feedback domain：scaffold → wire → migration → REST CRUD → revert）
> 於 2026-07-03 通過。

以 `internal/notification/` 為範例（Phase 2），複製時改模組名稱即可。

## 1. 文件

- [ ] 在 `docs/ARCHITECTURE.md` 記下新模組的邊界、REST 介面與 AuthZ actions

## 2. 資料庫

- [ ] `internal/{service}/migrations/00000N_*.up.sql` + `.down.sql`
- [ ] 掛載到 `docker-compose.yml` `postgres` init（下一個序號）
- [ ] 加入 `test/e2e/platform_test.go` `mustLoadMigrations()` 檔案列表

## 3. 程式（三層）

- [ ] `domain/` — 實體與不變量
- [ ] `repository/` — 介面 + Postgres（或 sqlc）
- [ ] `service/` — Use case + **`authz.Authorizer` 在 service**
- [ ] `handler/` — 僅 HTTP 轉譯 + `response` envelope

## 4. AuthZ

- [ ] `internal/pkg/authz/authorizer.go` 新增 action 常數
- [ ] `rbac_authorizer.go` 規則
- [ ] `internal/pkg/authz/policies/authz.rego` + `make opa-test`

## 5. 接線

- [ ] `internal/platform/router.go` — 註冊 routes
- [ ] `internal/platform/app.go` — `BuildApp` 組裝
- [ ] `cmd/server/providers.go` + `wire.go` — wire providers
- [ ] 執行 `go generate` / 更新 `wire_gen.go`

## 6. BFF（可選）

- [ ] `apps/bff/graph/schema.graphql` 新增 types/operations
- [ ] `gqlgen generate` + resolver 呼叫 `domainapi` client

## 7. 測試

- [ ] `internal/{service}/service/*_test.go` — AuthZ + 核心行為
- [ ] `go test ./...`、必要時 E2E 擴充

## 8. 驗收

- [ ] 新 service **不**在 BFF/Next 寫業務規則
- [ ] 從複製到綠燈 **≤ 1～2 人日**（熟悉後）
