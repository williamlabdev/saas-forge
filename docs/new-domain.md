# 快速新增領域:`make new-domain`

在這個 monolith 裡用一行指令長出一個新領域,形狀與既有領域(`notification` 等)一致:完整 CRUD、authz、驗證、同 transaction 的 outbox 事件、handler/router、migration、service 測試。

## 用法

```bash
make new-domain NAME=invoice
# 不規則複數:
make new-domain NAME=person PLURAL=people
```

`NAME` 用小寫單數英文字母。產生器會:
- 在 `internal/<name>/` 產生 domain / service / repository / handler / migrations
- 自動挑下一個 migration 編號(掃描 `internal/*/migrations` 取最大值 +1)
- 若 `internal/<name>/` 已存在則跳過 scaffold、只補接線
- **自動接線**(預設):在 `cmd/server/{providers,wire,router}.go`、`internal/platform/router.go` 與 `internal/platform/app.go`(`BuildApp`,e2e 測試用的手接線版本)的錨點註解處插入 import、provider、`wire.Build` 條目、router 參數與 `Routes()` 呼叫

底層腳本是 `scripts/new-domain.sh`,樣板在 `scripts/_domain-template/`(token 規則見其中 `TOKENS.md`)。樣板目錄以底線開頭,讓 Go 工具鏈(`go build ./...`、`go vet`、wire 的 package loader)忽略它——否則樣板裡未取代的 `__TOKEN__` import 會讓整個 module build 失敗。

## 自動接線(預設開啟)

產生器會自動把新領域接進 wiring。接線採「錨點註解 + 純附加插入」,具備:
- **idempotent**:重跑同名領域不會重複插入(已接的會 `skip`)
- **可選關閉**:`make new-domain NAME=invoice WIRE=0`(等同 `--no-wire`),改回只印手動步驟
- **不碰 `wire_gen.go`**:那是產生檔

接線後**必做兩步**:

1. `make wire`(重新產生 `wire_gen.go`;若有用到 mock 再 `make mocks`)
2. 套用 migration,然後 `make fmt`(見下方注意)、`go build ./...`、`go test ./...`

authz:預設 `AUTHZ_MODE=allow` 下直接通過;若用 rbac/opa,需為 `invoice:list|read|create|update|delete` 這些 action 加規則。

> **注意一(格式)**:自動插入的 import 放在錨點位置,可能不符合 gofmt/goimports 的字母排序——**請在產生後跑一次 `goimports -w` 或 `make fmt`** 讓 import 歸位。注意別讓格式工具把 `// new-domain:imports` 錨點移走,否則下次自動接線會插錯位置。
>
> **注意二(編譯驗證)**:此環境沒有 Go toolchain,產生器是以「對齊真實 `notification`/`user` 檔案」確保慣例一致;**請在本地跑 `make wire && go build ./...` 做最終驗證**。

### 若要關閉自動接線(手動接線)

`make new-domain NAME=invoice WIRE=0` 後,依腳本印出的 NEXT STEPS 手動加:providers 三個 provider、wire.Build 三條 + `wire.Bind`、`provideAppRouter`/`platform.NewRouter` 的 handler 參數與 `invoiceH.Routes(r)`。

## 設計選擇(可日後調整)

- 採 hand-written pgx(與 `notification` 一致),不走 sqlc,所以 `new-domain` 一步到位、不需 codegen。日後若要 sqlc,可另做樣板變體。
- outbox 採 `user.Create` 的**同 transaction** enqueue 寫法(`Begin → insert → EnqueueTx → Commit`),已避開先前 review 抓到的「分離 transaction」雙寫不一致問題。
- 範例 schema 是最小欄位(`id/owner_id/name/status/created_at/updated_at`),產生後依需求增改。

## 業務例外與錯誤回應(business exceptions)

錯誤型別是 `internal/pkg/errors.AppError`(stable `Code` + `Message` + HTTP status,可選 `Details`)。分層慣例:

- **框架層 sentinel** 留在 `internal/pkg/errors`:`ErrNotFound`(`NOT_FOUND`)、`ErrUnauthorized`、`ErrForbidden`——跨領域共用,不綁特定領域。
- **領域業務例外** 宣告在各領域的 `internal/<domain>/service/errors.go`,用 `apperrors.New("DOMAIN_REASON", msg, status)`。code 命名慣例:**`DOMAIN_REASON`**(screaming snake、領域前綴),例如 `USER_EMAIL_TAKEN`、`INVOICE_INVALID_STATUS_TRANSITION`。產生器已在每個新領域長出 `internal/<domain>/service/errors.go` 範本。

可用能力:

- `apperrors.Is(err, ErrXxx)` **以 code 比對**,即使被 `Wrap` 包裹仍比得中(`AppError.Is` 自訂方法)。
- `err.WithDetail(k, v)` / `WithDetails(m)` 附帶結構化 details(如欄位驗證錯誤);它會**複製**而非改動原 sentinel,可安全用在 package-level 例外上。
- `response.Error(w, err)` 會輸出 `code` / `message` / `details`,並在 **5xx 時記一行 log**(非 `AppError` 一律對映 500 並記錄,避免伺服器錯誤無聲消失)。

## 未來升級到獨立 service(option 2)

目前是 option 1(同 repo 加領域)。等基礎設施就緒、確定需要獨立部署時,再抽出 `platform-kit` 共享 module(errors / authz / outbox / pagination 等現在住在 `internal/pkg/` 的東西),並把 `new-domain` 機制沿用到各 service。
