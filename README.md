# AGBridge

给 **Antigravity 桌面版（Windows）** 加第三方模型的本地代理。第三方模型和订阅自带的模型一起出现在原生的模型下拉框里，可以随时切换；没选第三方模型时，所有流量原样转发给 Google，订阅模型的行为不变。

- 支持三种上游协议：**Gemini 原生 API**（Google AI Studio 或兼容 Gemini 格式的中转站）、**OpenAI 兼容**（DeepSeek、OpenRouter、硅基流动、Ollama……）、**Anthropic Messages**
- 单个 exe，托盘常驻，浏览器里的控制台负责配置
- 不改 Antigravity 本体代码，只替换一个可执行文件，随时一键还原

## 工作原理

```
Antigravity.exe ──启动──▶ language_server.exe（AGBridge shim）
                                │ 改写 --cloud_code_endpoint 等参数指向 127.0.0.1:50999
                                ▼
                     language_server.agb-patched.exe（原版副本）
                                │
                                ▼
                  AGBridge 代理 127.0.0.1:50999
                   ├─ fetchAvailableModels → 转发 Google，再把第三方模型塞进模型列表
                   ├─ streamGenerateContent（第三方模型）→ 转成上游协议 → 结果包回 Cloud Code 格式
                   └─ 其它所有请求 → 原样转发 Google（包括订阅模型的生成）
```

1. Antigravity 用写死的参数启动 `resources\bin\language_server.exe`，并把 Cloud Code 地址传给它。AGBridge 把这个 exe 换成一个很小的 shim：shim 把地址参数改成本地代理，再启动真正的语言服务器，stdin/stdout/退出码都原样透传。
2. 从 2.0.6 起，语言服务器拉取模型列表时用的是二进制里写死的 `https://daily-cloudcode-pa.googleapis.com`，不看参数。所以 AGBridge 会生成一份**补丁副本**，把这个地址替换成等长的本地地址（`http://127.0.0.1:50999/_agbd/xxxx…`）。原版文件原样备份。
3. 代理在 Google 返回的模型列表里追加第三方模型。每个模型占用一个语言服务器里预留的空编号 `MODEL_PLACEHOLDER_M<n>`（2.25.0 里有 M0–M1650），从高位往下分配，并避开 Google 已经在用的编号。
4. Antigravity 用这个模型发请求时，代理把 Cloud Code 的请求外壳拆掉，按提供方协议发出去，流式结果再包成 `{"response": …}` 的格式返回。
5. 指向自定义地址后，语言服务器会跳过按账号层级切换 Cloud Code 主机这一步（`UpdateEndpointURL`）。代理会读取 `loadCodeAssist` 的结果来替它补上：GCP 条款的付费层级走 production，其余走 daily。

如果 AGBridge 没在运行，shim 会先在后台把它拉起来；拉不起来时直接运行原版语言服务器，参数不做任何改动，Antigravity 照常可用，只是看不到第三方模型。

## 使用

1. 运行 `agbridge.exe`。托盘会出现图标，浏览器自动打开控制台 `http://127.0.0.1:50999/ui/`。
2. **添加提供方**：选协议，填 Base URL 和 API Key。
   - Gemini：填到域名即可，例如 `https://generativelanguage.googleapis.com` 或中转站地址（会自动补 `/v1beta`）。认证方式默认是 `x-goog-api-key` 头；有的中转站要用 `Bearer` 或 `?key=`，可以在这里切换。
   - OpenAI 兼容：填到 `/v1` 为止。
3. **添加模型**：点“从提供方获取模型列表”，可以一次勾选多个；也可以直接手填上游模型名。保存后点“测试”，确认模型能正常回答。
4. 点 **接入 / 修复**。Antigravity 开着也可以接入，正在运行的语言服务器不受影响。
5. **完全退出 Antigravity**（托盘里也要退出）再打开，就能在模型下拉框里选第三方模型了。

语言服务器只在启动时拉取一次模型列表，所以之后增删、停用模型，也要重启 Antigravity 才会反映到下拉框里。需要重启时，控制台会给出提示。

托盘菜单里可以开启 **开机自启**。即使没开，shim 也会在 Antigravity 启动时自动拉起 AGBridge。

## 卸载

在控制台点 **卸载接入**：原版 `language_server.exe` 会被还原，补丁副本和 shim 配置会被删除。如果 Antigravity 正开着，正在用的文件会先改名放在一边，等下次接入、卸载或自动修复时再删除。之后可以直接删掉 `agbridge.exe` 和 `%APPDATA%\agbridge`。

## 文件位置

| 内容 | 位置 |
| --- | --- |
| 配置（API Key 用 Windows DPAPI 加密） | `%APPDATA%\agbridge\config.json` |
| 运行日志 / shim 日志 | `%APPDATA%\agbridge\agbridge.log`、`shim.log` |
| 接入后新增的文件 | `<Antigravity>\resources\bin\language_server.agb-orig.exe`（原版备份）、`language_server.agb-patched.exe`、`agbridge-shim.json` |

## Antigravity 更新之后

Antigravity 自动更新时会整个替换安装目录，接入随之失效，但 Antigravity 本身照常可用。AGBridge 每 5 秒检查一次相关文件；只要之前接入过，文件稳定下来、并且确认 `language_server.exe` 已经完整写完，就会自动重新接入（Antigravity 开着也行）。

更新完 Antigravity 会立刻重新启动，这时用的还是没接入的语言服务器，所以**更新后还要再重启一次 Antigravity**，控制台会提示。也可以随时手动点“接入 / 修复”。

Windows 不允许覆盖或删除正在运行的 exe，但允许改名。接入和卸载正是利用这一点，在 Antigravity 运行时把占用中的文件改名挪开，名字形如 `*.agb-old-<时间戳>`；不再占用后，它们会被自动清理。

## 设置项

- **上游代理**：`auto`（默认，依次看环境变量和 Windows 系统代理）、`direct`，或者 `http://` / `socks5://` 地址。每个提供方还可以单独设置，比如国内中转站设成 `direct`。
- **思维签名**（Gemini 提供方）：如果对话里先用了订阅版 Gemini 再切到第三方 Gemini，历史里的 `thoughtSignature` 可能被第三方拒绝。默认的“自动”模式遇到这种拒绝时，会换成官方提供的占位签名重试一次。
- **推理强度**：OpenAI 兼容上游会收到 `reasoning_effort`；Anthropic 会开启 extended thinking；Gemini 会收到 `thinkingLevel`。Antigravity 给第三方模型发的思考预算总是 0（它按原生模型列表里的字段来定），所以 AGBridge 不照搬这个值。Gemini 3 及以后只认 `thinkingLevel`，2.5 只认 `thinkingBudget`：被上游拒绝时会自动换另一种写法重试，并记住这个模型该用哪种。不指定时由模型自己决定思考深度。

## 已知限制

- 只验证过 Windows 桌面版 **2.25.0** 的语言服务器能用补丁副本正常启动并接受本地地址。其它版本需要重新验证。
- 在“WSL 模式”下，语言服务器运行在 WSL 里，这时不会经过 shim。
- 第三方模型用的是 Antigravity 发出的 Gemini 格式工具定义；工具调用能不能稳定工作，取决于上游模型本身的能力。
- 对话压缩（checkpoint 摘要）仍由 Antigravity 选用自己的内置模型完成，消耗的是订阅额度。
- 这是对官方内部接口的非官方接入，Antigravity 更新后可能失效。是否符合 Google 的服务条款，请自行判断。

## 构建

```bash
./build.sh            # 输出 dist/agbridge.exe（内嵌 shim）
go test ./...
```

非 Windows 平台下，`go run ./cmd/agbridge` 会以无界面模式运行，方便开发调试。用环境变量 `AGBRIDGE_HOME` 可以换一个配置目录。

## 参考

- [vahapogut/antigravity-add-model](https://github.com/vahapogut/antigravity-add-model)：同类项目（Node/Electron），通过改 `app.asar` 和 VS Code 版 IDE 的设置来接入。AGBridge 的模型列表注入格式和占位编号分配参考了它。
