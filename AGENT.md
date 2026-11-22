# MCBOT-GO

mcbot-go是一款 go 语言编写的简洁插件，除 gorilla/websocket、golang.org/x/image、golang.org/x/term 外不使用其他第三方库，代码应简洁，可读性高，在修改或修复 BUG 时，应该修改其逻辑而不是打补丁做补充，否则代码会越来越冗余。

在实现时请选择轻量级实现方式，无需功能过度多的代码，本工具的实现方式如下：

## 实现方式

`https://bot.q.qq.com/wiki/develop/api-v2/autogen/api/`是官方文档，不要使用官方的 go-sdk，参照其中的http请求维护一个 `api.go` 文件，相当于我们自己维护一个sdk。

同时维护一个 `tcp.go` 文件，用于 tcp 的一些请求。

我们的工具接受群聊或私聊中的 `/server` 指令，子指令有 `add` `del`；每个群或私聊是一个独立会话：

添加一个 acovia 名称的服务器，地址指向 mc.acovia.net (支持srv解析)，按会话存储于当前工作目录的 `data/<session-id>/server.json` 文件内

```txt
/server add acovia mc.acovia.net
```

删除名称为 acovia 的服务器：

```txt
/server del acovia
```

然后可以接受群聊或私聊中的 `/ls [名称]` 指令，不指定名称时会列出所有的服务器：

读取 `data/<session-id>/server.json` 会话配置，根据参数向服务器发送 api.go 里的对应请求函数，向一个 mc 服务器发送 ping 请求，返回的函数里应当有版本号，motd，在线的玩家列表，使用 `image` 库渲染成图片，应当考虑边界情况，比如如何处理过长的列表，此处应该增加图片的高度以适配，然后发送到当前会话。

```txt
/ls
```

或

```txt
/ls acovia
```

功能就这么简单。


主配置为当前工作目录的 `config/mcbot-go.json`；新增 `init` 引导式创建工作目录。

群聊和私聊均支持 `/ls` 和 `/server`，私聊同样发送图片；每个群/私聊的服务器列表互相隔离。
