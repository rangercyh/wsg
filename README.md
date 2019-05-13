# wsg

websocket转socket

用go原生的tcp连接来接收连接，然后加了max conn的限制。

加了一个goroutine池，指定了worker的数量，listen改用了epoll事件循环，一定程度避免了ddos攻击。

accept后创建跟game的tcp连接。之后读写消息也是用的相同goroutine池，读写事件来了异步调用，免得阻碍了事件循环，所以读写要加锁，防止重入。

客户端消息过来流式拷贝到服务器的tcp；服务器消息过来，还是扫描去掉socket字节头，消息内容按websocket帧发回client的tcp。
