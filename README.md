# wsg

websocket转socket

用go原生的tcp连接来接收连接，然后加了accept max conn的限制。

加了一个goroutine池，指定了worker的数量，listen改用了epoll事件循环，一定程度避免了ddos攻击。

accept后创建跟game的tcp连接。之后读写消息也是用的相同goroutine池。

客户端消息过来流式拷贝到服务器的tcp；服务器消息过来，还是扫描去掉socket字节头，消息内容按websocket帧发回client的tcp。

# todo

- 增加首条协议鉴权，虽然要求网关参与逻辑，但是可以有效减少非法连接进来、断开、进来导致 TIME_WAIT 过多，占用系统资源的情况
- 另一个优化思路是让网关跟 game 保持 N 条 tcp 长连接，这就要求 game 端接收连接跟收消息的逻辑进行修改，session 不再跟 fd 绑定，而是用 uid 之类的进行消息区分，game 总体改动不大，但是这依然要求网关参与逻辑，例如玩家下线，需要网关解析 game 的下线消息，断开客户端连接
- 考虑协程池的执行顺序，如果能够有手段保证执行顺序，可以在读写事件处理函数里不对读写进行实际io操作，读写事件来了异步调用读写，免得阻碍了事件循环，所以读写要加锁，防止重入
- 增加限流限速的能力，考虑到目前读写消息跟accept新连接共用相同goroutine池，其实已经有一定限流能力，对于转发写速度暂时没觉得有必要做流量或者流速控制，如果要做，可以参考：

## 令牌桶算法限流

令牌桶算法最初来源于计算机网络. 在网络传输数据时, 为了防止网络拥塞, 需**限制流出网络的流量**, 使流量以比较均匀的速度向外发送. 令牌桶算法就实现了这个功能, 可控制发送到网络上数据的数目, 并允许突发数据的发送. 令牌桶算法是网络流量整形(Traffic Shaping)和速率限制(Rate Limiting)中最常使用的一种算法. 典型情况下, 令牌桶算法用来控制发送到网络上的数据的数目, 并允许突发数据的发送.

令牌桶以恒定的速率产生令牌. 传输数据需要消耗令牌. 依据数据量大小消耗等值数量的令牌. 控制令牌的生产速度即可进行网络速率限制.

## 代码实现

在 Go 语言中, 如果需要限制每单位时间的操作速度, 最便捷的方式是使用 `time.Ticker`(适用于每秒几十次操作的速率), 它和令牌桶模型几乎完全一致--按照固定速率产生令牌. 下述代码实现了一个限制 I/O 速度的 `CopyRate()` 函数.

```go
package copyrate

import (
    "io"
    "time"
)

func CopyRate(dst io.Writer, src io.Reader, bps int64) (written int64, err error) {
    throttle := time.NewTicker(time.Second)
    defer throttle.Stop()

    var n int64
    for {
        n, err = io.CopyN(dst, src, bps)
        if n > 0 {
            written += n
        }
        if err != nil {
            if err == io.EOF {
                err = nil
            }
            break
        }
        <-throttle.C // rate limit our flows
    }
    return written, err
}
```

测试: 复制文件且在复制过程中限制复制速度

```go
package main

import (
    "copyrate"
    "log"
    "os"
    "time"
)

func main() {
    src, err := os.Open("/tmp/foo.tar")
    if err != nil {
        log.Fatalln(err)
    }
    defer src.Close()

    dst, err := os.OpenFile("/tmp/bar.tar", os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
    if err != nil {
        log.Fatalln(err)
    }
    defer dst.Close()

    tic := time.Now()
    n, err := copyrate.CopyRate(dst, src, 1024*1024*10)
    toc := time.Since(tic)
    log.Println(n, err, toc)
}
```

源文件大小是 271M, bps 限制为 10M/S, 复制过程总耗时 27.02s. 最后记得 md5sum 一下两份文件, 确认 `CopyRate` 函数逻辑正常:

```sh
$ md5sum foo.tar bar.tar
ff90c9f1d438f80ce6392ff5d79da463 foo.tar
ff90c9f1d438f80ce6392ff5d79da463 bar.tar
```

### 参考

- [1] Go: RateLimiting [https://github.com/golang/go/wiki/RateLimiting](https://github.com/golang/go/wiki/RateLimiting)

## go rate 包限流

Go 语言中的 golang.org/x/time/rate 包采用了令牌桶算法来实现速率限制

这个包的核心部分在 Limit 类型和 NewLimiter接口

```go
// Limit 定义了事件的最大频率。
// Limit 被表示为每秒事件的数量。
// 值为0的Limit不允许任何事件。
type Limit float64

// 返回一个新的Limiter实例，
// 事件发生率为r，并允许至多b个令牌爆发。
func NewLimiter(r Limit, b int) *Limiter
```

rate 包还定义了一个有用的帮助函数 Every，将 time.Duration 转换为Limit对象

```go
// Every 将事件之间的最小时间间隔转换为 Limit。
func Every(interval time.Duration) Limit
```

创建 Limiter 对象后，我们使用 Wait,Allow或 Reserve 方法来阻塞我们的请求，直到获得访问令牌:

```go
// WaitN(ctx, 1)的简写。
func (lim *Limiter) Wait(ctx context.Context)

// WaitN 会发生阻塞直到 lim 允许的 n 个事件执行。
// 如果 n 超过了令牌池的容量大小则报错。
// 如果 ctx 被取消或等待时间超过了 ctx 的超时时间则报错。
func (lim *Limiter) WaitN(ctx context.Context, n int) (err error)

// AllowN(time.Now(), 1)的简写。
func (lim *Limiter) Allow() bool

// AllowN 标识在时间 now 的时候，n 个事件是否可以同时发生，
// 意思就是 now 的时候是否可以从令牌池中取 n 个令牌，
func (lim *Limiter) AllowN(now time.Time, n int) bool

// ReserveN(time.Now(), 1)的简写。
func (lim *Limiter) Reserve() *Reservation

// ReserveN 返回对象 Reservation ，标识调用者需要等多久才能等到 n 个事件发生，
// 意思就是 等多久令牌池中至少含有 n 个令牌。
func (lim *Limiter) ReserveN(now time.Time, n int) *Reservation

// eg:
r := lim.ReserveN(time.Now(), 1)
if !r.OK() {
    // Not allowed to act! Did you remember to set lim.burst to be > 0 ?
    return
}
time.Sleep(r.Delay())
Act()
```

- 如果要使用context 的截止日期或 cancel 方法的话，使用 WaitN。
- 如果需要在事件超出频率的时候丢弃或跳过事件，就使用 Allow,否则使用 Reserve 或 Wait。
- 如果事件发生的频率是可以由调用者控制的话，可以用 ReserveN 来控制事件发生的速度而不丢掉事件。

## 使用 rate 包来实现一个简单的 http 限流中间件

```go
package main

import (
    "net/http"

    "golang.org/x/time/rate"
)

var limiter = rate.NewLimiter(2, 5)

func limit(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if !limiter.Allow() {
            http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
            return
        }
        next.ServeHTTP(w, r)
    })
}

func okHandler(w http.ResponseWriter, r *http.Request) {
    w.Write([]byte("OK\n"))
}

func main() {
    mux := http.NewServeMux()
    mux.HandleFunc("/", okHandler)
    http.ListenAndServe(":4000", limit(mux))
}
```

测试

```bash
bash:~ $ while true; do
> curl http://localhost:4000/
> sleep 0.1s
> done
OK
OK
OK
OK
OK                 //首次爆发5个请求
OK
Too Many Requests
Too Many Requests
Too Many Requests
OK                 //之后每秒2个请求
Too Many Requests
Too Many Requests
Too Many Requests
OK
...
```

稍微修改下这个程序还可以实现 用户粒度的限流，基于IP地址或API密钥等标识符为每个用户实施速率限制器

### 参考

- [1] http限流中间件 [https://github.com/didip/tollbooth](https://github.com/didip/tollbooth)
