# CMS 使用手冊(PDF 產線)

給使用者看的手冊,不是給開發者的架構文件。開發向的文件在 `docs/ARCHITECTURE.md`
與 `docs/GETTING_STARTED.md`(架構決策紀錄只在內部 repo)。

## 重建 PDF

```bash
./docs/handbook/build.sh
```

輸出 `SaaSForge-CMS-使用手冊.pdf`(A4)。用 headless Chrome 直接列印,**不需要 pandoc /
LaTeX / wkhtmltopdf 這類文件工具鏈**——但需要 Chrome 或 Chromium。`build.sh` 會依序找
macOS 的 `/Applications/Google Chrome.app/...` 與 `/Applications/Chromium.app/...`,再找
`PATH` 上的 `google-chrome`、`google-chrome-stable`、`chromium`、`chromium-browser`;
都找不到就報錯中止,不會默默產出半成品。裝在別的位置就自己指:

```bash
CHROME=/path/to/chrome ./docs/handbook/build.sh
```

`handbook.html` 與 PDF 都在 `.gitignore` 裡(它們是產出物),所以**不會進版控,也不會
出現在公開 repo** ——公開副本只有各章 `.html` 與這支 `build.sh`,PDF 由讀者自己產。
那就是上面那組跨系統搜尋存在的理由:對拿到公開副本的人來說,跑這支腳本是取得成品的
唯一途徑,寫死一條 macOS 路徑等於把他們擋在門外。

## 檔案

| 檔案 | 內容 |
|---|---|
| `00-cover.html` | 封面與目錄 |
| `01`–`09-*.html` | 各章。章順序寫在 `build.sh` 裡 |
| `style.css` | 列印版型(A4、分頁規則、表格、提示框) |
| `build.sh` | 串接成單一 HTML 再印成 PDF |
| `handbook.html` | 產生出來的中間檔,可用瀏覽器直接預覽 |

改內容就改對應章節的 `.html`,再跑一次 `build.sh`。

## 一個踩過的坑:字型

**不要用 `PingFang TC`。** macOS 的 headless Chrome 印 PDF 時,PingFang 的字
**完全不會出現**——不是變成豆腐,是整段文字消失,而且 `pdftotext` 也抽不到,
所以肉眼不檢查會以為排版正常。`Heiti TC` 正常,目前的字型堆疊是:

```
"Helvetica Neue", Helvetica, "Heiti TC", "Hiragino Sans", sans-serif
```

改字型後請務必**把 PDF 轉成圖片實際看過**再交出去:

```bash
pdftoppm -png -r 80 -f 1 -l 3 SaaSForge-CMS-使用手冊.pdf /tmp/pg
```
