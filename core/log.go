package core

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"time"

	suiLog "github.com/Maoyangui/m-ui/logger"

	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing/common"
	F "github.com/sagernet/sing/common/format"
	"github.com/sagernet/sing/common/observable"
	"github.com/sagernet/sing/service/filemanager"
)

type PlatformWriter struct{}

func (p PlatformWriter) DisableColors() bool {
	return true
}
func (p PlatformWriter) WriteMessage(level log.Level, message string) {
	enqueueLog(level, "", message)
}

// 数据面日志不能同步写。sing-box 是在处理连接的 goroutine 里直接调日志的,底下是 stderr → journald → 磁盘
// (Ubuntu 上 rsyslog 还会再抄一份到 /var/log/syslog)。副机上按 info 逐条记连接,一小时几万行,journald
// 堆到几 GB;磁盘一顿,所有正在处理的连接跟着顿几秒 —— 用户看到的就是"隧道卡一下、面板卡一下"
// (真机 /proc/pressure/io 的 full 停顿能占到每分钟 5% 以上)。
// 这里把日志放进有界队列,单独一个 goroutine 往外写:内核那头只是入队,队列满了就丢、丢了多少每分钟报一次。
type logEntry struct {
	level log.Level
	tag   string
	msg   string
}

var (
	logQueue   = make(chan logEntry, 8192)
	logDropped atomic.Int64
)

func init() { go drainLogQueue() }

func enqueueLog(level log.Level, tag, msg string) {
	select {
	case logQueue <- logEntry{level: level, tag: tag, msg: msg}:
	default:
		logDropped.Add(1)
	}
}

func drainLogQueue() {
	var lastReport time.Time
	for e := range logQueue {
		emitLog(e.level, e.tag, e.msg)
		if n := logDropped.Load(); n > 0 && time.Since(lastReport) > time.Minute {
			logDropped.Add(-n)
			lastReport = time.Now()
			suiLog.Warning("数据面日志来不及写,丢弃了 ", n, " 条(磁盘慢或日志量大;日志页可把数据面级别调到 warn)")
		}
	}
}

func emitLog(level log.Level, tag, msg string) {
	args := []interface{}{msg}
	if tag != "" {
		args = []interface{}{tag, msg}
	}
	switch level {
	case log.LevelInfo:
		suiLog.Info(args...)
	case log.LevelWarn:
		suiLog.Warning(args...)
	case log.LevelPanic:
	case log.LevelFatal:
	case log.LevelError:
		suiLog.Error(args...)
	default:
		suiLog.Debug(args...)
	}
}

func NewFactory(options log.Options) (log.Factory, error) {
	logOptions := options.Options

	if logOptions.Disabled {
		return log.NewNOPFactory(), nil
	}

	var logWriter io.Writer
	var logFilePath string

	switch logOptions.Output {
	case "":
		logWriter = options.DefaultWriter
		if logWriter == nil {
			logWriter = os.Stderr
		}
	case "stderr":
		logWriter = os.Stderr
	case "stdout":
		logWriter = os.Stdout
	default:
		logFilePath = logOptions.Output
	}
	logFormatter := log.Formatter{
		BaseTime:         options.BaseTime,
		DisableColors:    logOptions.DisableColor || logFilePath != "",
		DisableTimestamp: !logOptions.Timestamp && logFilePath != "",
		FullTimestamp:    logOptions.Timestamp,
		TimestampFormat:  "-0700 2006-01-02 15:04:05",
	}
	factory := NewDefaultFactory(
		options.Context,
		logFormatter,
		logWriter,
		logFilePath,
	)
	if logOptions.Level != "" {
		logLevel, err := log.ParseLevel(logOptions.Level)
		if err != nil {
			return nil, common.Error("parse log level", err)
		}
		factory.SetLevel(logLevel)
	} else {
		factory.SetLevel(log.LevelTrace)
	}
	return factory, nil
}

var _ log.Factory = (*defaultFactory)(nil)

type defaultFactory struct {
	ctx        context.Context
	formatter  log.Formatter
	writer     io.Writer
	file       *os.File
	filePath   string
	level      log.Level
	subscriber *observable.Subscriber[log.Entry]
	observer   *observable.Observer[log.Entry]
}

func NewDefaultFactory(
	ctx context.Context,
	formatter log.Formatter,
	writer io.Writer,
	filePath string,
) log.ObservableFactory {
	factory := &defaultFactory{
		ctx:        ctx,
		formatter:  formatter,
		writer:     writer,
		filePath:   filePath,
		level:      log.LevelTrace,
		subscriber: observable.NewSubscriber[log.Entry](128),
	}
	return factory
}

func (f *defaultFactory) Start() error {
	if f.filePath != "" {
		logFile, err := filemanager.OpenFile(f.ctx, f.filePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			return err
		}
		f.writer = logFile
		f.file = logFile
	}
	return nil
}

func (f *defaultFactory) Close() error {
	return common.Close(
		common.PtrOrNil(f.file),
		f.subscriber,
	)
}

func (f *defaultFactory) Level() log.Level {
	return f.level
}

func (f *defaultFactory) SetLevel(level log.Level) {
	f.level = level
}

func (f *defaultFactory) Logger() log.ContextLogger {
	return f.NewLogger("")
}

func (f *defaultFactory) NewLogger(tag string) log.ContextLogger {
	return &observableLogger{f, tag}
}

func (f *defaultFactory) Subscribe() (subscription observable.Subscription[log.Entry], done <-chan struct{}, err error) {
	return f.observer.Subscribe()
}

func (f *defaultFactory) UnSubscribe(sub observable.Subscription[log.Entry]) {
	f.observer.UnSubscribe(sub)
}

type observableLogger struct {
	*defaultFactory
	tag string
}

func (l *observableLogger) Log(ctx context.Context, level log.Level, args []any) {
	level = log.OverrideLevelFromContext(level, ctx)
	if level > l.level {
		return
	}
	msg := F.ToString(args...)
	enqueueLog(level, l.tag, msg) // 入队即返,写盘由 drainLogQueue 那个 goroutine 慢慢做
	if (l.filePath != "" || l.writer != os.Stderr) && l.writer != nil {
		message := l.formatter.Format(ctx, level, l.tag, msg, time.Now())
		l.writer.Write([]byte(message))
	}
}

func (l *observableLogger) Trace(args ...any) {
	l.TraceContext(context.Background(), args...)
}

func (l *observableLogger) Debug(args ...any) {
	l.DebugContext(context.Background(), args...)
}

func (l *observableLogger) Info(args ...any) {
	l.InfoContext(context.Background(), args...)
}

func (l *observableLogger) Warn(args ...any) {
	l.WarnContext(context.Background(), args...)
}

func (l *observableLogger) Error(args ...any) {
	l.ErrorContext(context.Background(), args...)
}

func (l *observableLogger) Fatal(args ...any) {
	l.FatalContext(context.Background(), args...)
}

func (l *observableLogger) Panic(args ...any) {
	l.PanicContext(context.Background(), args...)
}

func (l *observableLogger) TraceContext(ctx context.Context, args ...any) {
	l.Log(ctx, log.LevelTrace, args)
}

func (l *observableLogger) DebugContext(ctx context.Context, args ...any) {
	l.Log(ctx, log.LevelDebug, args)
}

func (l *observableLogger) InfoContext(ctx context.Context, args ...any) {
	l.Log(ctx, log.LevelInfo, args)
}

func (l *observableLogger) WarnContext(ctx context.Context, args ...any) {
	l.Log(ctx, log.LevelWarn, args)
}

func (l *observableLogger) ErrorContext(ctx context.Context, args ...any) {
	l.Log(ctx, log.LevelError, args)
}

func (l *observableLogger) FatalContext(ctx context.Context, args ...any) {
	l.Log(ctx, log.LevelFatal, args)
}

func (l *observableLogger) PanicContext(ctx context.Context, args ...any) {
	l.Log(ctx, log.LevelPanic, args)
}

func (f *defaultFactory) AttachPlatformWriter(platformWriter log.PlatformWriter) {
}
