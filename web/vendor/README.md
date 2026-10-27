# 第三方静态文件

不引用外网 CDN（开发文档 §7）。升级时替换文件并更新下表，校验值与 npm 元数据的 `dist.integrity` 一致。

| 文件 | 来源 | 版本 | 许可证 | 校验（npm 包） |
|---|---|---|---|---|
| `echarts.min.js` | npm `echarts` 包的 `dist/echarts.min.js` | 6.1.0 | Apache-2.0（`echarts-LICENSE.txt`、`echarts-NOTICE.txt`） | `sha512-q0yaFPggC9FUdsWH4blavRWFmxdrIodbkoKNAjJudAI6CA9gNPxHtV2RcZNEepZVlk4yvBYkOkbk6HIVpIyHZA==` |

`echarts.min.js` 里唯一的 `new Function` 只在解析地图 GeoJSON 且浏览器没有 `JSON.parse` 时执行，看板不用地图，CSP 不放开 `unsafe-eval`。
