/*
分布式文件共享系统

将数据库由sqlite3换成ql2，保证整个项目为纯go，方便交叉编译。

*/

package main

import (
    "fmt"
    "io/ioutil"
    "net"
    "time"
    "strings"
    "os"
    "bytes"
    "encoding/binary"
    "encoding/hex"
    "crypto/sha1"
    "io"
    "flag"
    "sync"
    "path/filepath"
    "database/sql"
    _ "modernc.org/ql/driver"
    "strconv"
    "errors"
    "sort"
    "github.com/remeh/sizedwaitgroup"
)

const ( //定义指令码，数据包第一个字节为指令码
    DOWNLOAD_FILE byte = 1 //下载文件，后面跟文件的key
    //FILE_SIZE byte = 2 //文件大小，后面跟文件大小（单位Byte）
    LEND_GLOBAL_DB_LOCK byte = 6 //借用全局数据库锁
    RETURN_GLOBAL_DB_LOCK byte = 7 //归还全局数据库锁
    ACK byte = 8 //表示收到信息
    SYNC_GLOBAL_DB byte = 9 //同步数据库指令，后面跟文件大小（uint64）和数据库内容
    UPLOAD_FILE byte = 10 //上传文件指令，后面跟文件的key+文件大小+文件内容
    DELETE_FILE byte = 11 //删除文件指令，后面跟文件的key
    SEND_GLOBAL_DB byte = 12 //发送全局数据库指令
    JOIN_CLUSTER byte = 13 //加入集群指令，后面跟服务器端口（uint16）
    GET_SERVER_LIST byte = 14 //下载服务器列表
    SYNC_SERVER_LIST byte = 15 //同步服务器列表
    SERVER_LOAD byte = 16 //服务器负载
)

const CLIENT_SHELL_HELP_MSG= //客户端命令行帮助信息
    `
    help：查看帮助
    ls：查看可下载的文件列表
        使用-l参数可以查看可下载的文件及其分块、分块所在的服务器
    get [filename]：下载文件
    put [filename]：上传文件
    del [filename]：删除文件
    exit：退出
    `

const FILE_BLOCK_SIZE=1024*1024*32 //文件分块大小，单位Byte
const FILE_READ_SIZE=1024*1024*2 //读取缓存大小
const (
    DB_TYPE="ql2"
    GLOBAL_DB_PATH="global.db"
)

/*
数据库同步方案：
需要写入数据库的节点先给所有节点发送通知，申请分布式锁
节点写入完成后，将数据库同步给所有节点
同步完成后释放分布式锁
如果要读取数据，需要在没有节点写入数据库的时候读取（如果读取时有节点要申请锁，读取完再给）

TODO：新的数据库结构：服务器加入集群的时候扫描本地文件块，文件块下载完成进行校验，断点续传，迁移等功能
TABEL key_server
(
key char(40),//key，即文件分块名，sha1字符串形式，共40字节
server varchar(255) //文件分块所在的服务器
)

TABEL file_key
(
filename varchar(255),//文件名
num int(4),//文件分块号，从0开始
key char(40),//key，即文件分块名，sha1字符串形式，共40字节
)

TABLE file
(
filename varchar(255),//文件名
hash varchar(255),//文件hash，非必须
deleted bool //文件是否已被删除
)
*/

//TODO:美化输出

/*
高可用
上传：选最优2个服务器上传
下载：随机选一个服务器
掉线服务器数据同步问题：建立长连接，发现自己掉线，重连后更新数据库和服务器列表。
*/

//var download_mission sync.WaitGroup //下载任务的WaitGroup
var download_mission=sizedwaitgroup.New(2) //最大同时下载任务为2
var upload_mission sync.WaitGroup //上传任务的WaitGroup
var global_server_list [] string //服务器列表，格式如“127.0.0.1::2333”
var global_db_lock_status int = FREE //全局数据库锁
var global_server_load uint8 = 0 //服务器负载

const ( //定义全局数据库锁状态
    FREE = 0 //没有节点在使用全局数据库锁
    READING = 1 //本节点在读取全局数据库
    LOAN = 2 //锁已借出
    USING = 3 //锁自己在用
    /*
    全局数据库锁使用思路：
    如果需要写入，则等待global_db_lock_status=FREE，
    然后向所有节点发出申请，等待回复后global_db_lock_status=USING，
    写入完成后向所有节点进行同步，同步完成后向所有节点释放锁，并global_db_lock_status=FREE。
    如果需要读取，则等待global_db_lock_status=FREE，
    然后global_db_lock_status=READING，读取完毕后global_db_lock_status=FREE。
    如果有服务器要借用锁，则等待global_db_lock_status=FREE，
    然后借用，global_db_lock_status=LOAN，还回来后global_db_lock_status=FREE。
    TODO：考虑一下两个节点同时借用的死锁问题。
    */
)

var enable_server = flag.Bool("enable_server", false, "Enable server.启用服务器。")
var port = flag.String("port", "2333", "Listening port.监听端口（启用服务器才有效）。")
var first_server = flag.Bool("first_server", false, "First server, disable server scan.集群首台服务器，不进行服务器列表扫描。")
var verbose = flag.Bool("v", true, "Verbose output.输出详细信息。")

func checkErr(err interface{}){
    if err != nil {
        fmt.Println("[ERROR]发生致命错误！以下是错误描述：")
        fmt.Println(err)
        os.Exit(1)
    }
}

func main() {
    flag.Parse()//读取命令行参数
    log("enable_server",*enable_server)
    log("port",*port)
    log("verbose",*verbose)

    //创建文件夹
    if(!isPathExists("./tmp")){os.Mkdir("./tmp", os.ModePerm)}
    if(!isPathExists("./storage")){os.Mkdir("./storage", os.ModePerm)}
    if(!isPathExists("./download")){os.Mkdir("./download", os.ModePerm)}

    //根据参数判断是否作为服务端启动
    if *enable_server {
        fmt.Println("[INFO]系统启动……")
        go tcpServer(*port)//启动服务器，接收客户端和其它服务器的消息
    }

    if !*first_server {
        refreshServerList()//刷新服务器列表
        //连接服务器
        fmt.Println("[INFO]连接服务器……")
        var connected_server string
        for _,server:= range global_server_list {
            tcpAddr, err := net.ResolveTCPAddr("tcp", server);checkErr(err)
            conn, err := net.DialTCP("tcp", nil, tcpAddr)
            if err!=nil {continue}
            fmt.Println("服务器连接成功：",server)
            connected_server=server
            fmt.Println("[INFO]更新共享文件数据……")
            sendInstruct(SEND_GLOBAL_DB,conn)
            err=reciveFile(GLOBAL_DB_PATH,conn)//下载文件
            if err!=nil {
                fmt.Println("[ERROR]下载文件失败")
                os.Exit(1)
            }
            //加入服务器集群
            if *enable_server {
                fmt.Println("[INFO]加入服务器集群……")
                bytes_buf := bytes.NewBuffer(make([]byte, 0))
                binary.Write(bytes_buf, binary.BigEndian, JOIN_CLUSTER)//1字节指令码
                server_port, err := strconv.ParseInt(*port, 10, 32);checkErr(err)
                binary.Write(bytes_buf, binary.BigEndian, uint16(server_port))//2字节端口号（uint16）
                conn.Write(bytes_buf.Bytes())
                //writeAll(conn,bytes_buf.Bytes())
                instruct := readInstruct(conn)
                if instruct==ACK {
                    fmt.Println("[INFO]服务器集群加入成功")
                }else{
                    fmt.Println("[ERROR]服务器集群加入失败，请检查端口映射")
                    os.Exit(1)
                }
            }
            //关闭连接并退出循环
            conn.Close()
            break
        }
        if connected_server=="" {
            if *enable_server {

            }else{
                fmt.Println("ERROR]没有连上任何服务器，请检查服务器列表！")
                return
            }
        }

        fmt.Println("[INFO]更新服务器列表……")
        tcpAddr, err := net.ResolveTCPAddr("tcp", connected_server);checkErr(err)
        conn, err := net.DialTCP("tcp", nil, tcpAddr)
        if err != nil {
            fmt.Println("[ERROR]更新服务器列表失败，请检查网络连接！")
            os.Exit(1)
        }
        sendInstruct(GET_SERVER_LIST,conn)
        err=reciveFile("server_list.txt",conn)//下载文件
        if err!=nil {
            fmt.Println("[ERROR]下载文件失败")
            os.Exit(1)
        }
        refreshServerList()//刷新服务器列表
        conn.Close()
    }else{
      if(!isPathExists(GLOBAL_DB_PATH)){
        fmt.Println("[INFO]没有数据库，新建中...")
        db, err := sql.Open(DB_TYPE, GLOBAL_DB_PATH);checkErr(err)
        tx, err := db.Begin();checkErr(err)
        _, err = tx.Exec(`CREATE TABLE keys (
        filename string,
        num int,
        key string,
        server string
        );`);checkErr(err)
        err = tx.Commit();checkErr(err)
        err = db.Close();checkErr(err)
        /*if err = db.Close(); err != nil {
            return
        }*/
        fmt.Println("[INFO]数据库新建完成...")
      }
      //TODO:加检测数据库里面是否存在数据表的步骤
    }

    if !*first_server {//发布自己存在的块
        fmt.Println("[INFO]更新数据库文件……")
        //TODO！！！！！！！！！
    }

    if *enable_server {
        fmt.Println("[INFO]服务器启动完成。")
    }else{
        go clientShell()//启用客户端命令行
    }

    for{
        time.Sleep(time.Hour)//死循环，任务交由其它goroutine执行
    }
}




func tcpServer(port string){//服务器goroutine，接收客户端和其它服务器的消息
    //启动服务器
    tcpAddr, err := net.ResolveTCPAddr("tcp",":"+port)
    tcpListener, err := net.ListenTCP("tcp",tcpAddr)
    if err != nil {
        fmt.Println("[ERROR]服务器启动错误：",err)
        panic("服务器启动错误")
    }
    //处理客户端传入连接
    ConnMap := make(map[string]*net.TCPConn)//使用Map来存储连接
    for{
        tcpConn, _ := tcpListener.AcceptTCP()
        defer tcpConn.Close()
        ConnMap[tcpConn.RemoteAddr().String()] = tcpConn
        fmt.Println("新的连接：",tcpConn.RemoteAddr().String())
        go clientHandle(tcpConn) //新建一个goroutine来处理客户端连接
    }
}


func clientHandle(conn net.Conn) {//客户端连接处理goroutine，处理客户端消息
    global_server_load++
    defer func(){global_server_load--}()
    defer conn.Close() //函数结束前关闭连接
    defer fmt.Println("连接断开：",conn.RemoteAddr().String()) //函数结束前输出提示
    //循环的处理客户的请求
    for {
        //http://blog.sina.com.cn/s/blog_9be3b8f10101lhiq.html
        //conn.SetReadDeadline(time.Now().Add(time.Second * 150)) //一定时间内客户端无数据发送则关闭连接，这个太小会导致客户端上传到一半中断。TODO：解决会中断到问题。
        //读取数据
        instruct := readInstruct(conn)
        if instruct==255 {break}
        /*instruct := make([]byte, 1) //初始化缓冲区，1字节
        _,err:=conn.Read(instruct)
        if err != nil {break}*/
        //fmt.Println(n,err)
        /*for{//不需要，read是阻塞的。
            n,err:=conn.Read(instruct)
            fmt.Println(n,err)
            if(n==0){
                time.Sleep(time.Millisecond*1000)
            }else{
                break
            }
        }*/
        //switch instruct[0] {//根据指令码做出选择
        switch instruct {//根据指令码做出选择
            case DOWNLOAD_FILE://下载文件
                key:=readKey(conn)//读取文件key
                log("[接收到指令]客户端下载文件：",key)
                sendFile("storage/"+key,conn)//发送文件
                /*
                文件下载交互流程：
                客户端连接服务端
                客户端发送指令DOWNLOAD_FILE+文件key
                服务端发送文件大小（文件不存在则返回0）+文件内容
                客户端接收文件，直到接收到文件大小
                客户端关闭连接
                服务端关闭连接
                */
            case LEND_GLOBAL_DB_LOCK://借用全局数据库锁
                log("[接收到指令]借出全局数据库锁")
                acquireGlobalLock_Loan()
                sendInstruct(ACK,conn)
            case RETURN_GLOBAL_DB_LOCK://归还全局数据库锁
                log("[接收到指令]归还全局数据库锁")
                releaseGlobalLock_Loan()
                sendInstruct(ACK,conn)
            case SYNC_GLOBAL_DB://同步全局数据库
                log("[接收到指令]开始同步全局数据库")
                err:=reciveFile(GLOBAL_DB_PATH,conn)
                if err!=nil {
                    fmt.Println("[ERROR]全局数据库同步出错")
                    break
                }
                sendInstruct(ACK,conn)
                fmt.Println("全局数据库同步完毕")
            case SYNC_SERVER_LIST://同步服务器列表
                log("[接收到指令]开始同步服务器列表")
                err:=reciveFile("server_list.txt",conn)
                if err!=nil {
                    fmt.Println("[ERROR]服务器列表同步出错")
                    break
                }
                sendInstruct(ACK,conn)
                refreshServerList()
                fmt.Println("服务器列表同步完毕")
            case UPLOAD_FILE:
                key := readKey(conn)//读取文件key
                log("[接收到指令]客户端上传文件：",key)
                err:=reciveFile("storage/"+key,conn)
                if err!=nil {
                    fmt.Println("[ERROR]客户端文件上传出错")
                    break
                }
                sendInstruct(ACK,conn)
                fmt.Println("客户端文件上传完毕")
                /*
                文件上传交互流程：
                客户端连接服务端
                客户端发送指令UPLOAD_FILE+文件key+文件大小+文件内容
                服务端接收文件，直到接收到文件大小，返回ACK
                客户端关闭连接
                服务端关闭连接
                */
            case DELETE_FILE:
                key := readKey(conn)
                log("[接收到指令]客户端删除文件：",key)
                os.Remove("storage/"+key)
                sendInstruct(ACK,conn)
                /*
                文件删除交互流程：
                客户端连接服务端
                客户端发送指令DELETE_FILE+文件key
                服务端删除文件，返回ACK
                客户端关闭连接
                服务端关闭连接
                */
            case SEND_GLOBAL_DB:
                log("[接收到指令]发送全局数据库")
                acquireGlobalLock_Read()
                sendFile(GLOBAL_DB_PATH,conn)
                releaseGlobalLock_Read()
                /*
                发送全局数据库交互流程：
                客户端连接服务端
                客户端发送指令SEND_GLOBAL_DB
                服务端返回文件大小（8字节）+全局数据库
                客户端关闭连接
                服务端关闭连接
                */
            case JOIN_CLUSTER:
                /*
                加入集群交互流程：
                客户端连接服务端
                客户端发送指令JOIN_CLUSTER+端口号
                服务端尝试连接，如果连接成功，更新服务器列表，返回ACK，否则返回0
                客户端关闭连接
                服务端关闭连接
                */
                log("[接收到指令]有服务器加入集群")
                //读取服务器端口
                data := make([]byte, 2)
                conn.Read(data)
                var server_port uint16
                binary.Read(bytes.NewBuffer(data), binary.BigEndian, &server_port)
                server:=strings.Split(conn.RemoteAddr().String(),":")[0]+":"+strconv.Itoa(int(server_port))
                //建立测试连接
                test_tcpAddr, err := net.ResolveTCPAddr("tcp", server);checkErr(err)
                test_conn, err := net.DialTCP("tcp", nil, test_tcpAddr)
                if err != nil {
                    //test_conn.Close()
                    fmt.Println("测试连接失败")
                    conn.Write([]byte{255})
                    continue //结束处理
                }
                fmt.Println("测试连接成功")
                test_conn.Close()
                //更新服务器列表
                file_datas, _ := ioutil.ReadFile("server_list.txt")
                if !strings.Contains(string(file_datas),server){
                    fmt.Println("新增服务器：",server)
                    file_server_list_strings:=strings.TrimSpace(string(file_datas))+"\r\n"+server
                    file_server_list, err := os.Create("server_list.txt");checkErr(err)
                    file_server_list.Write([]byte(file_server_list_strings))
                    file_server_list.Close()
                }else{
                    fmt.Println("服务器已在列表中：",server)
                }
                sendInstruct(ACK,conn)//返回ACK
                syncServerList()//向所有节点同步服务器列表
                refreshServerList()//刷新服务器列表
            case GET_SERVER_LIST:
                log("[接收到指令]请求同步服务器列表")
                sendFile("server_list.txt",conn)
                /*
                同步服务器列表交互流程：
                客户端连接服务端
                客户端发送指令GET_SERVER_LIST
                服务端发送文件大小（8字节）+服务器列表
                客户端关闭连接
                服务端关闭连接
                */
            case SERVER_LOAD:
                log("[接收到指令]查询服务器负载：",global_server_load)
                conn.Write([]byte{global_server_load})
            case 255:
        }
    }
}


func clientShell(){//客户端命令行
    fmt.Println("启用客户端命令行，输入help可以查看帮助。")
    for{
        fmt.Printf("$ ")
        var command string
        var parameter [3] string
        fmt.Scanf("%s %s %s %s", &command, &parameter[0], &parameter[1], &parameter[2])
        //fmt.Printf("%s %s %s %s\n", command, parameter[0], parameter[1], parameter[2])
        switch command {
            case "help"://帮助
                fmt.Println(CLIENT_SHELL_HELP_MSG)
            case "exit"://退出
                os.Exit(0)
            case "get"://下载文件
                /*
                下载文件流程：
                先从文件数据库查询文件名对应的文件分块数量和校验码
                然后根据文件分块所在的服务器，智能选择每个分块的下载服务器
                所有文件分块下载完成后，合并成一个完整文件
                */
                //先从文件数据库读取文件名对应的key和服务器
                fmt.Println("查找数据库……")
                acquireGlobalLock_Read()
                db, err := sql.Open(DB_TYPE, GLOBAL_DB_PATH);checkErr(err)//连接全局数据库
                //从数据库中读取文件名并新建下载任务
                rows, err := db.Query(`SELECT num,key,server FROM keys WHERE filename=$1 ORDER BY num`,parameter[0]);checkErr(err)
                var key_list [] string
                var server_key_pair [] ServerKeyPair
                for rows.Next() {
                    var key,server string
                    var num int //这个不会用到
                    if err = rows.Scan(&num,&key,&server); err != nil {
                        rows.Close()
                        break
                    }
                    key_list=append(key_list,key)
                    server_key_pair=append(server_key_pair,ServerKeyPair{Server:server,Key:key})
                }
                err = db.Close();checkErr(err)
                releaseGlobalLock_Read()
                //key去重
                key_list=RemoveDuplicatesAndEmpty(key_list)
                //提交下载任务
                //难点：实现智能选择服务器，多线程下载
                //理想实现：看服务器带宽情况
                //实际实现：根据服务器连接数进行评分
                for i,key := range key_list{//遍历出每个key及其对于的服务器，然后选择最佳服务器进行下载
                    var key_server_list [] string //服务器列表
                    //遍历出每个key及其对于的服务器
                    for _,server_key := range server_key_pair{
                        if server_key.Key==key{
                            key_server_list=append(key_server_list,server_key.Server)
                        }
                    }
                    //选择最佳服务器
                    best_server_load:=uint8(255)//服务器负载
                    var best_server string
                    for _,server := range key_server_list{
                        server_load:=getServerLoad(server)
                        if server_load<=best_server_load {
                            best_server=server
                            best_server_load=server_load
                        }
                    }
                    fmt.Println("提交下载任务",i)
                    download_mission.Add()
                    go downloadFile(key, best_server)
                }
                //等待下载完毕
                fmt.Println("等待下载完成……")
                download_mission.Wait()
                //合并文件
                fmt.Println("合并文件块……")
                file_full, _ := os.Create("download/"+parameter[0])
                for _,key := range key_list {
                    file_piece, err := os.Open("tmp/"+key);checkErr(err)
                    buf:=make([]byte, getFileSize("tmp/"+key))
                    file_piece.Read(buf)//全部读取
                    file_full.Write(buf)
                    file_piece.Close()
                    os.Remove("tmp/"+key)
                }
                file_full.Close()
                fmt.Println("文件下载成功")
            case "ls"://查看可下载的文件列表
                acquireGlobalLock_Read()
                db, err := sql.Open(DB_TYPE, GLOBAL_DB_PATH);checkErr(err)//连接全局数据库
                //直接从数据库中读取文件名并打印
                if parameter[0]=="-l" {
                    rows, err := db.Query(`SELECT * FROM keys`);checkErr(err)
                    for rows.Next() {
                        var filename,key,server string
                        var num int
                        if err = rows.Scan(&filename,&num,&key,&server); err != nil {
                            rows.Close()
                            break
                        }
                        fmt.Println(filename,num,key,server)
                    }
                }else{
                    rows, err := db.Query(`SELECT DISTINCT filename FROM keys`);checkErr(err)
                    for rows.Next() {
                        var filename string
                        if err = rows.Scan(&filename); err != nil {
                            rows.Close()
                            break
                        }
                        fmt.Println(filename)
                    }
                }
                err = db.Close();checkErr(err)
                releaseGlobalLock_Read()
            case "put"://上传文件 TODO：多线程上传
                /*
                上传文件流程：
                判断文件大小，如果超过分块数量，则切割成块
                计算所有分块的sha1值并重命名
                选择服务器（负载均衡或者hash的方式）并上传文件块
                全部上传完毕后申请全局数据库锁
                将文件信息写入全局数据库
                释放全局数据库锁
                */
                file_path:=parameter[0]
                fmt.Println("文件路径：",file_path)
                _ , filename := filepath.Split(file_path)
                fmt.Println("文件名：",filename)
                file_size:=getFileSize(file_path)
                fmt.Println("文件大小：",file_size)
                var key_list [] string //key数组
                if file_size>FILE_BLOCK_SIZE{
                    file_pieces_num:=int(file_size/FILE_BLOCK_SIZE)+1
                    fmt.Println("文件超过文件块大小，需要分块。分块数量：",file_pieces_num)
                    f, err := os.Open(file_path);checkErr(err)
                    buf_size:=FILE_BLOCK_SIZE
                    for i:=0;i<file_pieces_num;i++ {
                        if int(file_size)-FILE_BLOCK_SIZE*i<FILE_BLOCK_SIZE {
                            buf_size=int(file_size)-FILE_BLOCK_SIZE*i
                        }
                        //fmt.Println(buf_size)
                        buf:=make([]byte, buf_size)
                        f.ReadAt(buf,int64(FILE_BLOCK_SIZE*i))//每次读取一个FILE_BLOCK_SIZE
                        key:=hex.EncodeToString(hashBytes(buf))//计算key
                        fmt.Println("第",i,"个key：",key)
                        key_list=append(key_list,key)
                        //写入文件
                        file_piece, err := os.Create("tmp/"+key);checkErr(err)
                        file_piece.Write(buf)
                        file_piece.Close()
                    }
                    f.Close()
                    fmt.Println("文件分块完成！")
                }else{
                    fmt.Println("文件小于等于文件块大小，无需分块。")
                    f, err := os.Open(file_path);checkErr(err)
                    buf:=make([]byte, file_size)
                    f.Read(buf)//全部读取
                    key:=hex.EncodeToString(hashBytes(buf))//计算key
                    fmt.Println("第0个key：",key)
                    key_list=append(key_list,key)
                    //写入文件
                    file_piece, err := os.Create("tmp/"+key);checkErr(err)
                    file_piece.Write(buf)
                    file_piece.Close()
                    f.Close()
                }
                //选择服务器并上传文件块
                //查询数据库，计算每个服务器的文件数量，从小到大排序，排序相同的按服务器字符串排序
                //将一个分块发送到两个服务器上，然后重复上面的步骤，查询最佳服务器并继续上传
                fmt.Println("准备上传文件分块……")
                for i,key := range key_list {
                    //选择服务器 TODO：性能待优化
                    fmt.Println("查找数据库……")
                    acquireGlobalLock_Read()
                    db, err := sql.Open(DB_TYPE, GLOBAL_DB_PATH);checkErr(err)//连接全局数据库
                    servers := map[string]int{} //ket为服务器ip，value为服务器上块的数量
                    for _,server := range global_server_list {
                        var num int
                        rows, err := db.Query(`SELECT * FROM keys WHERE server=$1`,server);checkErr(err)
                        for rows.Next() {
                            num+=1
                        }
                        servers[server]=num
                        fmt.Println(server,num)
                    }
                    err = db.Close();checkErr(err)
                    releaseGlobalLock_Read()
                    //将server的key数量进行排序
                    servers_sorted:=sortMapByValue(servers)
                    fmt.Println("服务器及其块数量：",servers_sorted)
                    //fmt.Println("最佳上传服务器：",servers_sorted[0].Key,servers_sorted[0].Value)
                    //fmt.Println("次佳上传服务器：",servers_sorted[1].Key,servers_sorted[1].Value)
                    //连接服务器
                    server_upload:=make([]string,2)
                    server_upload_point:=0;
                    for{//上传第一个副本
                        tcpAddr, err := net.ResolveTCPAddr("tcp", servers_sorted[server_upload_point].Key);checkErr(err)
                        conn, err := net.DialTCP("tcp", nil, tcpAddr)
                        if err != nil {
                            fmt.Println("服务器连接失败：",tcpAddr)
                            if(server_upload_point>=servers_sorted.Len()){
                                fmt.Println("[ERROR]所有服务器连接失败，没有可上传的服务器！")
                                os.Exit(1)
                            }
                            server_upload_point++
                        }else{//如果服务器正常，就开始上传。TODO：两个副本同时上传（多线程）
                            fmt.Println("上传第",i,"个文件分块：",key,servers_sorted[server_upload_point].Key)
                            upload_mission.Add(1)
                            uploadFile(key,conn)
                            server_upload[0]=servers_sorted[server_upload_point].Key
                            server_upload_point++
                            break
                        }
                    }
                    for{//上传第二个副本
                        if(server_upload_point>=servers_sorted.Len()){
                            fmt.Println("[WARN]只连上一个服务器，该文件分块没有多副本！")
                            break
                        }
                        tcpAddr, err := net.ResolveTCPAddr("tcp", servers_sorted[server_upload_point].Key);checkErr(err)
                        conn, err := net.DialTCP("tcp", nil, tcpAddr)
                        if err != nil {
                            fmt.Println("服务器连接失败：",tcpAddr)
                            if(server_upload_point>=servers_sorted.Len()){
                                fmt.Println("[WARN]只连上一个服务器，该文件分块没有多副本！")
                                break
                            }
                            server_upload_point++
                        }else{//如果服务器正常，就开始上传。TODO：两个副本同时上传（多线程）
                            fmt.Println("上传第",i,"个文件分块：",key,servers_sorted[server_upload_point].Key)
                            upload_mission.Add(1)
                            uploadFile(key,conn)
                            server_upload[1]=servers_sorted[server_upload_point].Key
                            break
                        }
                    }
                    upload_mission.Wait()
                    //删除文件
                    err = os.Remove("tmp/"+key)
                    if err!=nil {
                        fmt.Println("[WARN]文件删除失败，可稍后手动删除。",err)
                    }
                    //写入全局数据库
                    fmt.Println("准备写入全局数据库……")
                    //申请全局数据库锁，TODO：避免死锁
                    acquireGlobalLock_Write()
                    //写入数据库
                    db, err = sql.Open(DB_TYPE, GLOBAL_DB_PATH);checkErr(err)//连接全局数据库
                    //stmt, _ := db.Prepare("INSERT INTO keys VALUES (?,?,?,?)")
                    //stmt.Exec(filename,i,key,min_key_nums_server)
                    tx, err := db.Begin();checkErr(err)
                    _, err = tx.Exec(`INSERT INTO keys VALUES ($1,$2,$3,$4);`,filename,i,key,servers_sorted[0].Key);checkErr(err)
                    if(server_upload[1]!=""){
                        _, err = tx.Exec(`INSERT INTO keys VALUES ($1,$2,$3,$4);`,filename,i,key,servers_sorted[1].Key);checkErr(err)
                    }
                    err = tx.Commit();checkErr(err)
                    log("插入数据。",)
                    err = db.Close();checkErr(err)
                    fmt.Println("数据库更新成功。")
                    //同步数据库到其它服务器
                    syncGlobalDatabase()
                    //归还全局数据库锁
                    releaseGlobalLock_Write()
                }
                fmt.Println("文件上传完毕！")
            case "del"://删除文件
                /*
                删除文件流程：
                申请全局数据库锁
                将文件信息从全局数据库中删除
                释放全局数据库锁
                通知对应的服务器删除文件块
                */
                fmt.Println("准备写入全局数据库……")
                //申请全局数据库锁，TODO：避免死锁
                acquireGlobalLock_Write()
                //查询key并删除
                db, err := sql.Open(DB_TYPE, GLOBAL_DB_PATH);checkErr(err)//连接全局数据库
                rows, err := db.Query(`SELECT key,server FROM keys WHERE filename = $1`,parameter[0]);checkErr(err)
                var key_list [] string
                for rows.Next() {
                    var key,server string
                    if err = rows.Scan(&key,&server); err != nil {
                        rows.Close()
                        break
                    }
                    fmt.Println(key,server)
                    key_list=append(key_list,key)
                }
                //db.Exec("DELETE FROM keys WHERE filename = '"+parameter[0]+"'")//删除文件
                tx, err := db.Begin();checkErr(err)
                _, err = tx.Exec(`DELETE FROM keys WHERE filename = $1`,parameter[0]);checkErr(err)//删除文件
                err = tx.Commit();checkErr(err)
                err = db.Close();checkErr(err)
                fmt.Println("数据库更新成功。")
                //同步数据库到其它服务器
                syncGlobalDatabase()
                //归还全局数据库锁
                releaseGlobalLock_Write()
                //通知对应的服务器删除文件块 TODO:待优化，只通知存在的服务器删除，通知失败则留着上线删除
                log("通知服务器删除文件……")
                for _,key := range key_list {
                    bytes_buf := bytes.NewBuffer(make([]byte, 0))
                    binary.Write(bytes_buf, binary.BigEndian, DELETE_FILE)
                    binary.Write(bytes_buf, binary.BigEndian, []byte(key))
                    sendDatasToAllServers(bytes_buf.Bytes())
                }
                fmt.Println("文件删除完毕！")
        }
    }
}























//hashFile利用hash算法将目标文件生成哈希值 https://blog.csdn.net/benben_2015/article/details/80146147
func hashFile(filePath string) []byte {
    f, err := os.Open(filePath);checkErr(err)
    defer f.Close()
    h := sha1.New()
    /*if _, err := io.Copy(h, f); err != nil {
        fmt.Println(err)
    }*/
    _, err = io.Copy(h, f);checkErr(err)
    //fmt.Println(hex.EncodeToString(h.Sum(nil)))
    return h.Sum(nil) //长度20Byte
}

func hashBytes(b []byte) []byte {
    h := sha1.New()
    h.Write(b)
    //fmt.Println(hex.EncodeToString(h.Sum(nil)))
    return h.Sum(nil) //长度20Byte
}

func getFileSize(filepath string) uint64 {//读取文件大小
    f, err := os.Open(filepath)
    if err != nil {
        fmt.Println("[ERROR]文件大小读取错误！",err)
        os.Exit(1)
    }
    file_size, err := f.Seek(0, os.SEEK_END);checkErr(err)
    return uint64(file_size)
}

func downloadFile(filename string, tcpAddrString string)error{//下载文件
    //连接服务器
    tcpAddr, err := net.ResolveTCPAddr("tcp", tcpAddrString);checkErr(err)
    conn, err := net.DialTCP("tcp", nil, tcpAddr)
    if err != nil {
        fmt.Println("服务器连接失败")
        return errors.New("服务器连接失败")
    }
    defer conn.Close()
    //发送下载请求
    sendInstruct(DOWNLOAD_FILE,conn)
    sendString(filename,conn)
    //下载文件
    err=reciveFile("tmp/"+filename,conn)
    if err!=nil {
        fmt.Println("[ERROR]下载文件失败")
        os.Exit(1)
    }
    //完成任务
    download_mission.Done()
    return nil
}

func sendDatasToAllServers(datas []byte){
    for _, server := range global_server_list{
        tcpAddr, err := net.ResolveTCPAddr("tcp", server);checkErr(err)
        conn, err := net.DialTCP("tcp", nil, tcpAddr)
        if err != nil {
            fmt.Println("服务器连接失败：",tcpAddr)
            continue
        }
        conn.Write(datas)
        //fmt.Println("数据发送成功：",datas)
        instruct := make([]byte, 1)
        conn.Read(instruct)
        //fmt.Println("数据读取成功：",instruct)
        var i byte
        binary.Read(bytes.NewBuffer(instruct), binary.BigEndian, &i)
        for{
            if i==ACK {
                break
            }
            time.Sleep(time.Millisecond*10)
        }
        conn.Close()
    }
}

func acquireGlobalLock_Write(){//请求全局数据库锁
    for{
        if global_db_lock_status==FREE{
            global_db_lock_status=USING
            sendDatasToAllServers([]byte{LEND_GLOBAL_DB_LOCK})
            fmt.Println("全局数据库锁申请成功。")
            break
        }
    }
}

func releaseGlobalLock_Write(){//释放全局数据库锁
    sendDatasToAllServers([]byte{RETURN_GLOBAL_DB_LOCK})
    global_db_lock_status=FREE //=releaseGlobalLock_Read()
    fmt.Println("全局数据库锁归还成功。")
}

func acquireGlobalLock_Read(){//请求全局数据库读取锁
    for{
        if global_db_lock_status==FREE{
            global_db_lock_status=READING
            break
        }
    }
}

func releaseGlobalLock_Read(){//释放全局数据库读取锁
    global_db_lock_status=FREE
}

func acquireGlobalLock_Loan(){//请求全局数据库锁，借出
    for{
        if global_db_lock_status==FREE{
            global_db_lock_status=LOAN
            break
        }
    }
}

func releaseGlobalLock_Loan(){//释放全局数据库锁(借出)
    global_db_lock_status=FREE
}

func syncGlobalDatabase(){
    bytes_buf := bytes.NewBuffer(make([]byte, 0))
    binary.Write(bytes_buf, binary.BigEndian, SYNC_GLOBAL_DB)
    binary.Write(bytes_buf, binary.BigEndian, getFileSize(GLOBAL_DB_PATH))
    file_datas, err := ioutil.ReadFile(GLOBAL_DB_PATH);checkErr(err)
    binary.Write(bytes_buf, binary.BigEndian, file_datas)
    sendDatasToAllServers(bytes_buf.Bytes())
    fmt.Println("数据库同步成功。")
}

func syncServerList(){
    bytes_buf := bytes.NewBuffer(make([]byte, 0))
    binary.Write(bytes_buf, binary.BigEndian, SYNC_SERVER_LIST)
    binary.Write(bytes_buf, binary.BigEndian, getFileSize("server_list.txt"))
    file_datas, err := ioutil.ReadFile("server_list.txt");checkErr(err)
    binary.Write(bytes_buf, binary.BigEndian, file_datas)
    sendDatasToAllServers(bytes_buf.Bytes())
    fmt.Println("数据库同步成功。")
}

func log(v ... interface{}){
    if *verbose {
        fmt.Println(v...)
    }
}

func refreshServerList(){//刷新服务器列表
    //读取服务器列表
    log("读取服务器列表")
    b, err := ioutil.ReadFile("server_list.txt");checkErr(err)
    //将文件内容转为字符串，去除首尾的空白字符，按换行切割（如果换行是linux，只要\n），结果为服务器IP数组
    global_server_list = strings.Split(strings.TrimSpace(string(b)), "\r\n")
    for i,server:= range global_server_list {
        fmt.Printf("服务器%d：%s\n", i,server)
    }
}

func reciveFile(file_path string, conn net.Conn)error{
    time_start:=time.Now()
    data := make([]byte, 8)
    conn.Read(data)
    var file_size uint64
    binary.Read(bytes.NewBuffer(data), binary.BigEndian, &file_size)
    log("文件大小：",file_size)
    var download_size uint64 = 0
    f, err := os.Create(file_path);checkErr(err)
    defer f.Close()
    for{
        data := make([]byte, FILE_READ_SIZE)
        n, err := conn.Read(data);
        if err != nil {
            fmt.Println("[WARN]文件下载出错",err)
            return err
        }
        f.Write(data[:n])
        download_size+=uint64(n)
        fmt.Printf("进度：%.2f\n",float32(download_size)*100/float32(file_size))
        if download_size==file_size{
            break
        }
    }
    fmt.Println("文件下载完毕")
    time_end:=time.Now()
    fmt.Printf("下载速度：%.3f MB/s\n",float64(float64(file_size)/1024/1024/time_end.Sub(time_start).Seconds()))
    return nil
}

func sendFile(file_path string, conn net.Conn){
    time_start:=time.Now()
    bytes_buf := bytes.NewBuffer(make([]byte, 0))
    file_size:=getFileSize(file_path)
    //binary.Write(bytes_buf, binary.BigEndian, FILE_SIZE)//1字节指令码
    binary.Write(bytes_buf, binary.BigEndian, file_size)//8字节文件大小（uint64）
    conn.Write(bytes_buf.Bytes())
    //发送文件 TODO：对大文件的优化。目前是全部读进内存。
    log("开始发送文件……")
    //file_datas, err := ioutil.ReadFile(file_path);checkErr(err)
    //conn.Write(file_datas)
    f,err:=os.Open(file_path);checkErr(err)
    defer f.Close()
    buf := make([]byte, FILE_READ_SIZE)
    for {
        n, err := f.Read(buf)
        if err != nil && err != io.EOF {
            fmt.Println("[WARN]文件发送出错",err)
            return
        }
        if 0 == n {
            break
        }
        writeAll(conn,buf[:n])
    }
    log("文件发送完毕！")//客户端接收完成后会关闭连接，服务器会自动关闭
    time_end:=time.Now()
    fmt.Printf("上传速度：%.3f MB/s\n",float64(float64(file_size)/1024/1024/time_end.Sub(time_start).Seconds()))
}

func readInstruct(conn net.Conn)byte{
    instruct := make([]byte, 1) //初始化缓冲区，1字节
    _,err:=conn.Read(instruct)
    if err==io.EOF{
        return 255
    }
    if err!=nil {
        fmt.Println("[ERROR]读取指令出错：",err)
        return 255
    }
    /*for {
        n, err := conn.Read(instruct) //从conn中读取数据，n为数据大小
        if err != nil && err.Error() != "EOF" {
            fmt.Println("[ERROR]读取指令出错：",err)
            return 255
            //panic("读取指令出错")
        }
        if n!=0 {
            break
        }
        time.Sleep(time.Millisecond*10)
    }*/
    return instruct[0]
}

func sendInstruct(instruct byte,conn net.Conn){
    conn.Write([]byte{instruct})
}

func sendString(strings string,conn net.Conn){
    conn.Write([]byte(strings))
}

func readKey(conn net.Conn)string{
    data := make([]byte, 40)//key长度40
    n, _ :=conn.Read(data)//读取key
    key:=string(data[0:n])//将key转成字符串
    return key
}

func isPathExists(path string)bool{
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	return false
}

func getServerLoad(server string)uint8{//获取服务器负载
    tcpAddr, err := net.ResolveTCPAddr("tcp", server);checkErr(err)
    conn, err := net.DialTCP("tcp", nil, tcpAddr)
    if err != nil {
        fmt.Println("服务器连接失败：",tcpAddr)
        return 255
    }
    conn.Write([]byte{SERVER_LOAD})
    server_load := make([]byte, 1)
    conn.Read(server_load)
    conn.Close()
    return uint8(server_load[0])
}

func uploadFile(key string,conn net.Conn){
    sendInstruct(UPLOAD_FILE,conn)
    sendString(key,conn)
    sendFile("tmp/"+key,conn)
    instruct:=readInstruct(conn)//等待服务端回应ACK
    if instruct==ACK {
        fmt.Println("上传成功：",key)
    }
    conn.Close()
    upload_mission.Done()
}

func writeAll(c net.Conn, b []byte)error{
    for{
        if len(b) <= 0{
            break
        }
        tmpn, err := c.Write(b)
        if err != nil{
            return err
        }
        b = b[tmpn:]
    }
    return nil
}






type ServerKeyPair struct {//服务器-key对
    Server   string
    Key string
}






//要对golang map按照value进行排序，思路是直接不用map，用struct存放key和value，实现sort接口，就可以调用sort.Sort进行排序了。
// A data structure to hold a key/value pair.
type Pair struct {
    Key   string
    Value int
}

// A slice of Pairs that implements sort.Interface to sort by Value.
type PairList []Pair

func (p PairList) Swap(i, j int)      { p[i], p[j] = p[j], p[i] }
func (p PairList) Len() int           { return len(p) }
func (p PairList) Less(i, j int) bool { return p[i].Value < p[j].Value }

// A function to turn a map into a PairList, then sort and return it.
func sortMapByValue(m map[string]int) PairList {
    p := make(PairList, len(m))
    i := 0
    for k, v := range m {
        p[i] = Pair{k, v}
        i+=1;
    }
    sort.Sort(p)
    return p
}

//数组去重
func RemoveDuplicatesAndEmpty(a []string) (ret []string){
    a_len := len(a)
    for i:=0; i < a_len; i++{
        if (i > 0 && a[i-1] == a[i]) || len(a[i])==0{
            continue;
        }
        ret = append(ret, a[i])
    }
    return
}
