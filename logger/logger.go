package logger

import (
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/op/go-logging"
)

var (
	logger    *logging.Logger
	logBuffer []struct {
		time  string
		level logging.Level
		log   string
	}
	bufMu    sync.Mutex
	disabled int32 // 1 = 面板"日志"页关闭了记录:不再写入环形缓冲
)

// SetEnabled 打开/关闭日志记录(仅影响面板内的环形缓冲;syslog/stderr 输出不受影响)。
func SetEnabled(on bool) {
	if on {
		atomic.StoreInt32(&disabled, 0)
	} else {
		atomic.StoreInt32(&disabled, 1)
	}
}

func Enabled() bool { return atomic.LoadInt32(&disabled) == 0 }

// Clear 清空环形缓冲。
func Clear() {
	bufMu.Lock()
	logBuffer = nil
	bufMu.Unlock()
}

// init 先装一个 stderr 默认日志器:CLI 子命令、测试与库调用不经过 InitLogger 也不会因 nil 崩溃。
func init() {
	l := logging.MustGetLogger("m-ui")
	backend := logging.NewLogBackend(os.Stderr, "", 0)
	format := logging.MustStringFormatter(`%{time:2006/01/02 15:04:05} %{level} - %{message}`)
	leveled := logging.AddModuleLevel(logging.NewBackendFormatter(backend, format))
	leveled.SetLevel(logging.INFO, "m-ui")
	l.SetBackend(leveled)
	logger = l
}

func InitLogger(level logging.Level) {
	newLogger := logging.MustGetLogger("m-ui")
	var err error
	var backend logging.Backend
	var format logging.Formatter

	_, inContainer := os.LookupEnv("container")
	if !inContainer {
		if _, statErr := os.Stat("/.dockerenv"); statErr == nil {
			inContainer = true
		}
	}
	if inContainer {
		backend = logging.NewLogBackend(os.Stderr, "", 0)
		format = logging.MustStringFormatter(`%{time:2006/01/02 15:04:05} %{level} - %{message}`)
	} else {
		backend, err = logging.NewSyslogBackend("")
		if err != nil {
			fmt.Println("Unable to use syslog: " + err.Error())
			backend = logging.NewLogBackend(os.Stderr, "", 0)
		}
		if err != nil {
			format = logging.MustStringFormatter(`%{time:2006/01/02 15:04:05} %{level} - %{message}`)
		} else {
			format = logging.MustStringFormatter(`%{level} - %{message}`)
		}
	}

	backendFormatter := logging.NewBackendFormatter(backend, format)
	backendLeveled := logging.AddModuleLevel(backendFormatter)
	backendLeveled.SetLevel(level, "m-ui")
	newLogger.SetBackend(backendLeveled)

	logger = newLogger
}

func GetLogger() *logging.Logger {
	return logger
}

func Debug(args ...interface{}) {
	logger.Debug(args...)
	addToBuffer("DEBUG", fmt.Sprint(args...))
}

func Debugf(format string, args ...interface{}) {
	logger.Debugf(format, args...)
	addToBuffer("DEBUG", fmt.Sprintf(format, args...))
}

func Info(args ...interface{}) {
	logger.Info(args...)
	addToBuffer("INFO", fmt.Sprint(args...))
}

func Infof(format string, args ...interface{}) {
	logger.Infof(format, args...)
	addToBuffer("INFO", fmt.Sprintf(format, args...))
}

func Warning(args ...interface{}) {
	logger.Warning(args...)
	addToBuffer("WARNING", fmt.Sprint(args...))
}

func Warningf(format string, args ...interface{}) {
	logger.Warningf(format, args...)
	addToBuffer("WARNING", fmt.Sprintf(format, args...))
}

func Error(args ...interface{}) {
	logger.Error(args...)
	addToBuffer("ERROR", fmt.Sprint(args...))
}

func Errorf(format string, args ...interface{}) {
	logger.Errorf(format, args...)
	addToBuffer("ERROR", fmt.Sprintf(format, args...))
}

func addToBuffer(level string, newLog string) {
	if atomic.LoadInt32(&disabled) == 1 {
		return
	}
	t := time.Now()
	bufMu.Lock()
	defer bufMu.Unlock()
	if len(logBuffer) >= 10240 {
		logBuffer = logBuffer[1:]
	}

	logLevel, _ := logging.LogLevel(level)
	logBuffer = append(logBuffer, struct {
		time  string
		level logging.Level
		log   string
	}{
		time:  t.Format("2006/01/02 15:04:05"),
		level: logLevel,
		log:   newLog,
	})
}

func GetLogs(c int, level string) []string {
	logLevel, _ := logging.LogLevel(level)

	type entry struct {
		time  string
		level logging.Level
		log   string
	}
	picked := make([]entry, 0, c+1)
	bufMu.Lock()
	for i := len(logBuffer) - 1; i >= 0 && len(picked) <= c; i-- {
		if logBuffer[i].level <= logLevel {
			picked = append(picked, entry{logBuffer[i].time, logBuffer[i].level, logBuffer[i].log})
		}
	}
	bufMu.Unlock()

	output := make([]string, 0, len(picked))
	for _, e := range picked { // 格式化在锁外做
		output = append(output, fmt.Sprintf("%s %s - %s", e.time, e.level, e.log))
	}
	return output
}
