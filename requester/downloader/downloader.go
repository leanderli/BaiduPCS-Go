package downloader

import (
	"context"
	"errors"
	"fmt"
	"github.com/iikira/BaiduPCS-Go/pcsverbose"
	"github.com/iikira/BaiduPCS-Go/requester"
	"github.com/iikira/BaiduPCS-Go/requester/rio"
	"io"
	"sync"
	"time"
)

// Downloader 下载
type Downloader struct {
	onExecute         func() //开始下载事件
	onFinish          func() //结束下载事件
	onPause           func() //暂停下载事件
	onResume          func() //恢复下载事件
	onCancel          func() //取消下载事件
	monitorCancelFunc context.CancelFunc

	sinceTime     time.Time
	durl          string
	writer        rio.WriteCloserAt
	client        *requester.HTTPClient
	config        *Config
	monitor       *Monitor
	instanceState *InstanceState
}

//MustCheck 遇到错误则panic
func (der *Downloader) MustCheck() {
	if der.config == nil {
		panic("config is nil")
	}
	if der.client == nil {
		panic("client is nil")
	}
}

//NewDownloader 初始化Downloader
func NewDownloader(durl string, writer rio.WriteCloserAt, config *Config) (der *Downloader) {
	der = &Downloader{
		durl:   durl,
		config: config,
		writer: writer,
	}
	if der.config == nil {
		der.config = NewConfig()
	}
	return
}

//SetClient 设置http客户端
func (der *Downloader) SetClient(client *requester.HTTPClient) {
	der.client = client
	der.MustCheck()
}

func (der *Downloader) lazyInit() {
	if der.client == nil {
		der.client = requester.NewHTTPClient()
	}
	if der.monitor == nil {
		der.monitor = &Monitor{
			status: NewDownloadStatus(),
		}
	}
}

//Execute 开始任务
func (der *Downloader) Execute() error {
	der.lazyInit()
	der.MustCheck()

	// 检测
	resp, err := der.client.Req("HEAD", der.durl, nil, nil)
	if resp != nil {
		defer resp.Body.Close()
	}
	if err != nil {
		return err
	}

	// 检测网络错误
	switch resp.StatusCode / 100 {
	case 2: // succeed
	case 4, 5: // error
		return errors.New(resp.Status)
	}

	acceptRanges := resp.Header.Get("Accept-Ranges")
	if resp.ContentLength <= 0 {
		acceptRanges = ""
	} else {
		acceptRanges = "bytes"
	}

	der.monitor.status.totalSize = resp.ContentLength

	var (
		req           = resp.Request
		durl, referer string
	)

	if req != nil {
		referer = req.Referer()
		durl = req.URL.String()
	}

	//load breakpoint
	err = der.initInstanceState()
	if err != nil {
		return err
	}

	instanceInfo := der.instanceState.Get()
	var (
		dlStatus *DownloadStatus
		ranges   []*Range
	)
	if instanceInfo != nil {
		dlStatus = instanceInfo.DlStatus
		ranges = instanceInfo.Ranges
	}

	if dlStatus != nil {
		der.monitor.status = dlStatus
	}

	// 数据处理
	isRange := ranges != nil && len(ranges) > 0
	if acceptRanges == "" { //不支持多线程
		der.config.parallel = 1
	} else if isRange {
		der.config.parallel = len(ranges)
	} else {
		der.config.parallel = der.config.MaxParallel
		if int64(der.config.parallel) > der.monitor.status.totalSize/int64(MinParallelSize) {
			der.config.parallel = int(der.monitor.status.totalSize/int64(MinParallelSize)) + 1
		}
	}

	der.config.cacheSize = der.config.CacheSize
	blockSize := der.monitor.status.totalSize / int64(der.config.parallel)

	// 如果 cache size 过高, 则调低
	if int64(der.config.cacheSize) > blockSize {
		der.config.cacheSize = int(blockSize)
	}

	pcsverbose.Verbosef("DEBUG: CREATED: parallel: %d, cache size: %d\n", der.config.parallel, der.config.cacheSize)

	der.monitor.workers = make([]*Worker, 0, der.config.parallel)

	// 数据平均分配给各个线程
	var (
		begin, end int64
		writeMu    = &sync.Mutex{}
		worker     *Worker
		writerAt   io.WriterAt
	)
	if der.writer == nil {
		writerAt = nil
	} else {
		writerAt = der.writer
	}

	workerInit := func(wer *Worker) {
		wer.SetClient(der.client)
		wer.SetCacheSize(der.config.cacheSize)
		wer.SetWriteMutex(writeMu)
		wer.SetReferer(referer)
	}

	for i := 0; i < der.config.parallel; i++ {
		worker = NewWorker(int32(i), durl, writerAt)
		workerInit(worker)

		// 分配线程
		if isRange {
			worker.SetRange(acceptRanges, ranges[i])
		} else {
			end = int64(i+1) * blockSize
			worker.SetRange(acceptRanges, &Range{
				Begin: begin,
				End:   end,
			})
			begin = end + 1
			if i == der.config.parallel-1 {
				worker.wrange.End = der.monitor.status.totalSize - 1
			}
		}
		der.monitor.Append(worker)
	}

	moniterCtx, moniterCancelFunc := context.WithCancel(context.Background())
	der.monitorCancelFunc = moniterCancelFunc

	der.monitor.SetInstanceState(der.instanceState)
	trigger(der.onExecute)
	der.monitor.Execute(moniterCtx)
	der.removeInstanceState()
	trigger(der.onFinish)
	return nil
}

//GetDownloadStatusChan 获取下载统计信息
func (der *Downloader) GetDownloadStatusChan() <-chan DlStatus {
	if der.monitor == nil {
		return nil
	}
	c := make(chan DlStatus)
	nowTime := time.Now()
	go func() {
		for {
			select {
			case <-der.monitor.completed:
				close(c)
				return
			default:
				der.monitor.status.timeElapsed = time.Since(nowTime)
				c <- der.monitor.status
				time.Sleep(1 * time.Second)
			}
		}
	}()
	return c
}

//Pause 暂停
func (der *Downloader) Pause() {
	if der.monitor == nil {
		return
	}
	trigger(der.onPause)
	der.monitor.Pause()
}

//Resume 恢复
func (der *Downloader) Resume() {
	if der.monitor == nil {
		return
	}
	trigger(der.onResume)
	der.monitor.Resume()
}

//Cancel 取消
func (der *Downloader) Cancel() {
	if der.monitor == nil {
		return
	}
	trigger(der.onCancel)
	trigger(der.monitorCancelFunc)
}

//PrintAllWorkers 输出所有的worker
func (der *Downloader) PrintAllWorkers() {
	if der.monitor == nil {
		return
	}
	fmt.Println(der.monitor.ShowWorkers())
}

//OnExecute 设置开始下载事件
func (der *Downloader) OnExecute(fn func()) {
	der.onExecute = fn
}

//OnFinish 设置结束下载事件
func (der *Downloader) OnFinish(fn func()) {
	der.onFinish = fn
}

//OnPause 设置暂停下载事件
func (der *Downloader) OnPause(fn func()) {
	der.onPause = fn
}

//OnResume 设置恢复下载事件
func (der *Downloader) OnResume(fn func()) {
	der.onResume = fn
}

//OnCancel 设置取消下载事件
func (der *Downloader) OnCancel(fn func()) {
	der.onCancel = fn
}
