# AGBridge

**给 Antigravity 桌面版加上第三方模型：它们和订阅自带的模型出现在同一个下拉框里，随时切换。**

[![test](https://github.com/anlidy/agbridge/actions/workflows/test.yml/badge.svg)](https://github.com/anlidy/agbridge/actions/workflows/test.yml)
[![release](https://img.shields.io/github/v/release/anlidy/agbridge)](https://github.com/anlidy/agbridge/releases/latest)
[![license](https://img.shields.io/github/license/anlidy/agbridge)](LICENSE)

简体中文 | [English](README_en.md)

![AGBridge 控制台](docs/console.png)

- **三种上游协议**：
  - Gemini 原生 API：Google AI Studio，或兼容 Gemini 格式的中转站。
  - OpenAI 兼容：DeepSeek、OpenRouter、硅基流动、阿里云百炼、本地 Ollama……
  - Anthropic Messages。
- **用的是原生下拉框**：第三方模型的切换方式和 Gemini、Claude 一样。还可以像原生 Gemini 那样，在下拉框里切换 Low / Medium / High 思考档位。
- **订阅模型不受影响**：没选第三方模型时，所有请求原样转发给 Google。
- **单个 exe**：托盘常驻，配置在浏览器控制台里完成。Antigravity 启动时会自动把它拉起来。
- **不改 Antigravity 本体代码**：只替换一个可执行文件，可以一键还原。Antigravity 自动更新后会自动重新接入。

## 下载

到 [Releases](https://github.com/anlidy/agbridge/releases/latest) 下载 `agbridge.exe`。

要求：

- Windows 10 / 11 x64。
- Antigravity **桌面版**，也就是独立安装的 Antigravity 应用，不是基于 VS Code 的 Antigravity IDE。
- 已在 2.25.0 和 2.28.0 上验证。

exe 没有代码签名，第一次运行时 Windows SmartScreen 可能会拦一下：点“更多信息 → 仍要运行”即可。不放心的话，可以[从源码构建](#从源码构建)。

## 快速上手

### 1. 运行 AGBridge

先把 `agbridge.exe` 放到一个以后不会删掉的位置，例如 `%LOCALAPPDATA%\Programs\agbridge\`，再双击运行。原因是：接入后，Antigravity 会按这个路径自动启动 AGBridge。以后挪了位置也不要紧，只要再手动运行一次，它会自己更新路径。

运行后托盘会出现图标，浏览器会打开控制台 `http://127.0.0.1:50999/ui/`。之后想再打开控制台，右键托盘图标选“打开控制台”就行。

### 2. 添加提供方

在“提供方”一栏点“添加提供方”，选择协议，再填 Base URL 和 API Key。Base URL 输入框里有常用地址可以直接选。

| 协议 | Base URL 示例 | 说明 |
| --- | --- | --- |
| Gemini | `https://generativelanguage.googleapis.com` | 用 [Google AI Studio](https://aistudio.google.com/apikey) 的 Key。中转站就填中转站的域名，程序会自动补 `/v1beta`。“Gemini 认证方式”按中转站的要求选：`x-goog-api-key` 头、`Bearer` 或 `?key=`。 |
| OpenAI 兼容 | `https://api.deepseek.com/v1`<br>`https://openrouter.ai/api/v1`<br>`http://127.0.0.1:11434/v1`（Ollama） | 填到 `/v1` 为止，程序会请求 `…/chat/completions`。 |
| Anthropic | `https://api.anthropic.com` | 程序会请求 `…/v1/messages`。 |

每个提供方都可以单独设置代理。比如全局走代理时，可以把国内中转站设成 `direct`。

### 3. 添加模型

在“模型”一栏点“添加模型”，选好提供方：

- 点“从提供方获取模型列表”，可以一次勾选多个模型；也可以直接手填上游模型名。
- “在 Antigravity 里显示的名字”可以留空，留空时用上游模型名。
- 保存后点“测试”，看到模型正常回答，就说明 Key、地址和网络都没问题。

### 4. 接入 Antigravity

在控制台顶部点 **接入 / 修复**，或者在托盘菜单里选“接入 / 修复 Antigravity”。Antigravity 开着也可以接入。

### 5. 重启 Antigravity，选择模型

**完全退出 Antigravity**（托盘里也要退出），然后重新打开。在模型下拉框里就能看到第三方模型了。

之后再增删模型，不需要重启：语言服务器大约每 6 分钟会重新拉一次模型列表。等不及的话，重启 Antigravity 就行。

## 思考档位

原生的 Gemini 在下拉框里可以切换 Low / Medium / High。原理是模型列表里有“Gemini 3.8 Flash (Low)”“(High)”这样几个独立条目，Antigravity 会把它们合并成一个模型，再加上档位切换。

在模型设置里勾选 **下拉框档位**，AGBridge 就会照这个约定为每个档位注入一个条目。请求时按所选档位设置推理强度：

- Gemini：`thinkingLevel`。
- OpenAI 兼容：`reasoning_effort`。
- Anthropic：extended thinking 的预算。

不同模型支持的档位不一样，比如 Gemini 4 Argon 就不支持 Minimal。勾之前可以先用“测试”按钮试一下。

不勾档位时，只注入一个条目，用“推理强度”这一项，留空表示由模型自己决定。

## 常见问题

**下拉框里没有第三方模型？**

1. 看控制台的“Antigravity 接入”：接入状态应该是“已接入”。如果提示“需要重启”，就完全退出 Antigravity 再打开。
2. 看“最近请求”：每次 Antigravity 拉取模型列表，都会有一条“模型列表 +N 个自定义模型”。如果一条都没有，说明 Antigravity 还没走 AGBridge，一般是因为接入后还没重启。
3. 确认模型是“启用”状态，提供方也存在。

**选了第三方模型，Antigravity 提示 “Agent execution terminated due to error”？**

去控制台“最近请求”里看这次生成的错误信息，里面写着上游返回的原话，例如 Key 无效、模型名写错、额度用完。AGBridge 会把第三方的 401 / 403 / 404 转成 400 再交给 Antigravity，这样 Antigravity 不会误以为是你的 Google 登录失效。

**Antigravity 更新后第三方模型没了？**

更新会整个替换安装目录，接入随之失效。AGBridge 只要在运行，几秒内就会自动重新接入。不过更新完 Antigravity 会立刻自己重启，那一次用的还是原版语言服务器，所以需要**再重启一次 Antigravity**。如果 AGBridge 当时没在运行，就手动运行它，或者点“接入 / 修复”。

**AGBridge 需要一直开着吗？**

不需要专门开着。接入后，Antigravity 启动语言服务器时，会先检查 AGBridge 在不在，不在就在后台把它拉起来。托盘菜单里也可以开启“开机自启”。万一 AGBridge 怎么也起不来，Antigravity 会直接运行原版语言服务器：照常可用，只是看不到第三方模型。

**会影响订阅模型和账号吗？**

选订阅模型时，请求原样转发给 Google，AGBridge 只在模型列表里追加条目。不过这是对官方内部接口的非官方接入，Antigravity 更新后可能失效；是否符合 Google 的服务条款，请自行判断。

**杀毒软件报毒？**

AGBridge 会替换另一个程序的 exe，又没有代码签名，被启发式规则盯上并不奇怪。代码都在这个仓库里，可以自己构建后使用。

## 卸载

在控制台点 **卸载接入**：原版 `language_server.exe` 会还原，补丁副本和 shim 配置会删除。Antigravity 开着也可以卸载。之后退出 AGBridge，删掉 `agbridge.exe` 和 `%APPDATA%\agbridge` 即可。如果开过开机自启，先在托盘菜单里关掉。

## 文件位置

| 内容 | 位置 |
| --- | --- |
| 配置（API Key 用 Windows DPAPI 加密，只有当前 Windows 用户能解开） | `%APPDATA%\agbridge\config.json` |
| 运行日志 / shim 日志 | `%APPDATA%\agbridge\agbridge.log`、`shim.log` |
| 接入后在 Antigravity 目录新增的文件 | `resources\bin\language_server.agb-orig.exe`（原版备份）、`language_server.agb-patched.exe`、`agbridge-shim.json` |

## 设置项

- **上游代理**：`auto`（默认）会依次看环境变量和 Windows 系统代理；`direct` 表示直连；也可以填 `http://` 或 `socks5://` 地址。
- **思维签名**（只对 Gemini 提供方）：如果对话里先用订阅版 Gemini，再切到第三方 Gemini，历史消息里的 `thoughtSignature` 可能被第三方拒绝。默认的“自动”模式遇到这种拒绝时，会换成官方提供的占位签名重试一次。
- **端口**：默认 `50999`，只监听 `127.0.0.1`。改端口后要重启 AGBridge，再点一次“接入 / 修复”。
- 控制台的管理接口需要令牌，令牌只嵌在控制台页面里；同时会校验 Host 头，防止网页借 DNS 重绑定访问。

## 工作原理

```
Antigravity.exe ──启动──▶ language_server.exe（AGBridge shim）
                                │ 把 --cloud_code_endpoint 等参数改成 127.0.0.1:50999
                                ▼
                     language_server.agb-patched.exe（原版副本）
                                │
                                ▼
                  AGBridge 代理 127.0.0.1:50999
                   ├─ fetchAvailableModels → 转发给 Google，再追加第三方模型
                   ├─ streamGenerateContent（第三方模型）→ 转成上游协议 → 结果包回 Cloud Code 格式
                   └─ 其它所有请求 → 原样转发给 Google
```

1. **替换语言服务器**：Antigravity 用写死的参数启动 `resources\bin\language_server.exe`。AGBridge 把它换成一个很小的 shim，shim 改写地址参数后，再启动真正的语言服务器。stdin、stdout 和退出码都原样透传。
2. **补丁副本**：拉取模型列表用的 `https://daily-cloudcode-pa.googleapis.com` 写死在二进制里，不受参数影响。所以 AGBridge 会生成一份补丁副本，把它替换成等长的本地地址。原版文件原样备份。
3. **注入模型列表**：每个第三方模型（或它的每个档位）占用语言服务器里一个预留的空编号 `MODEL_PLACEHOLDER_M<n>`，从高位往下分配，并且避开 Google 已经在用的编号。
4. **转发生成请求**：选第三方模型时，代理拆掉 Cloud Code 的请求外壳，按提供方的协议发出去，再把流式结果包回 `{"response": …}` 格式。
5. **补上层级路由**：指向自定义地址后，语言服务器会跳过按账号层级切换 Cloud Code 主机这一步。代理读取 `loadCodeAssist` 的结果替它补上。
6. **运行中替换文件**：Windows 不允许覆盖正在运行的 exe，但允许改名。所以接入、卸载、自动修复都可以在 Antigravity 运行时进行：占用中的文件先改名放到一边（`*.agb-old-*`），不再占用后自动清理。
7. **自动修复**：每 5 秒检查一次文件。文件稳定下来、并且确认新的 `language_server.exe` 已经完整写完（检查到签名表结尾），才会重新接入。

## 已知限制

- 只在 Windows 桌面版 2.25.0 和 2.28.0 上验证过，其它版本可能需要调整。
- 在 Antigravity 的“WSL 模式”下，语言服务器运行在 WSL 里，不会经过 shim。
- 第三方模型收到的是 Antigravity 的 Gemini 格式工具定义。Agent 的工具调用能不能稳定工作，取决于上游模型本身的函数调用能力。
- 对话压缩（checkpoint 摘要）等后台任务，仍由 Antigravity 选用自己的内置模型完成。
- 控制台目前只有中文界面。

## 从源码构建

需要 Go 1.27+，在 Linux、macOS 或 Windows（Git Bash）上都能交叉编译：

```bash
./build.sh            # 先编译 shim，再把它嵌进 dist/agbridge.exe
go test ./...
```

非 Windows 平台上，`go run ./cmd/agbridge serve` 会以无界面模式运行，方便调试代理和控制台。环境变量 `AGBRIDGE_HOME` 可以换一个配置目录。

## 参考

- [vahapogut/antigravity-add-model](https://github.com/vahapogut/antigravity-add-model)：同类项目，通过修改 `app.asar` 和 VS Code 版 IDE 的设置来接入。AGBridge 的模型列表格式和占位编号分配参考了它。
- [YashjitPal/BetterGravity](https://github.com/YashjitPal/BetterGravity)：模型列表里 `thinkingBudget` 与思考档位的对应关系。

## 许可

[Apache-2.0](LICENSE)。本项目与 Google 无关，Antigravity 是 Google 的产品。
