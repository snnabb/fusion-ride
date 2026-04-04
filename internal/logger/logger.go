package logger

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Level 定义日志级别。
type Level int

const (
	DEBUG Level = iota
	INFO
	WARN
	ERROR
	FATAL
)

var levelNames = [...]string{"DBG", "INF", "WRN", "ERR", "FTL"}
var levelColors = [...]string{"\033[36m", "\033[32m", "\033[33m", "\033[31m", "\033[35m"}

const (
	reset      = "\033[0m"
	maxLogSize = 5 * 1024 * 1024 // 5 MB
)

// Logger 提供 Console + File 双输出、自动轮转的分级日志。
type Logger struct {
	mu       sync.Mutex
	filePath string
	file     *os.File
	minLevel Level
	isTTY    bool
	buf      []LogEntry // 最近 2000 条供管理面板查看
	maxBuf   int
}

// LogEntry 是一条结构化日志。
type LogEntry struct {
	Time    time.Time `json:"time"`
	Level   string    `json:"level"`
	Message string    `json:"message"`
	Caller  string    `json:"caller,omitempty"`
}

// New 创建日志实例。filePath 为空则仅输出到控制台。
func New(filePath string) *Logger {
	l := &Logger{
		filePath: filePath,
		minLevel: DEBUG,
		isTTY:    isTerminal(os.Stdout),
		maxBuf:   2000,
	}

	if filePath != "" {
		dir := filepath.Dir(filePath)
		os.MkdirAll(dir, 0755)
		f, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err == nil {
			l.file = f
		}
	}

	return l
}

func (l *Logger) log(level Level, format string, args ...any) {
	if level < l.minLevel {
		return
	}

	now := time.Now()
	msg := fmt.Sprintf(format, args...)

	// 调用者信息（仅 WARN+）
	var caller string
	if level >= WARN {
		if _, file, line, ok := runtime.Caller(2); ok {
			caller = fmt.Sprintf("%s:%d", filepath.Base(file), line)
		}
	}

	entry := LogEntry{
		Time:    now,
		Level:   levelNames[level],
		Message: msg,
		Caller:  caller,
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	// 缓冲供管理面板
	l.buf = append(l.buf, entry)
	if len(l.buf) > l.maxBuf {
		l.buf = l.buf[len(l.buf)-l.maxBuf:]
	}

	// Console 输出
	ts := now.Format("15:04:05.000")
	if l.isTTY {
		callerSuffix := ""
		if caller != "" {
			callerSuffix = fmt.Sprintf(" \033[90m(%s)%s", caller, reset)
		}
		fmt.Fprintf(os.Stdout, "%s%s %s│%s %s%s\n",
			levelColors[level], levelNames[level], reset,
			reset, msg, callerSuffix)
	} else {
		fmt.Fprintf(os.Stdout, "%s %s %s\n", ts, levelNames[level], msg)
	}

	// File 输出
	if l.file != nil {
		callerSuffix := ""
		if caller != "" {
			callerSuffix = " (" + caller + ")"
		}
		fmt.Fprintf(l.file, "%s %s %s%s\n", ts, levelNames[level], msg, callerSuffix)
		l.rotateIfNeeded()
	}
}

func (l *Logger) Debug(format string, args ...any) { l.log(DEBUG, format, args...) }
func (l *Logger) Info(format string, args ...any)  { l.log(INFO, format, args...) }
func (l *Logger) Warn(format string, args ...any)  { l.log(WARN, format, args...) }
func (l *Logger) Error(format string, args ...any) { l.log(ERROR, format, args...) }

func (l *Logger) Fatal(format string, args ...any) {
	l.log(FATAL, format, args...)
	os.Exit(1)
}

// Recent 返回最近 n 条日志。
func (l *Logger) Recent(n int) []LogEntry {
	l.mu.Lock()
	defer l.mu.Unlock()

	if n > len(l.buf) {
		n = len(l.buf)
	}
	out := make([]LogEntry, n)
	copy(out, l.buf[len(l.buf)-n:])
	return out
}

// Content 返回日志文件全部内容（用于下载）。
func (l *Logger) Content() ([]byte, error) {
	if l.filePath == "" {
		return nil, fmt.Errorf("未配置日志文件")
	}
	return os.ReadFile(l.filePath)
}

// Clear 清空日志文件和缓冲。
func (l *Logger) Clear() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.buf = l.buf[:0]

	if l.file != nil {
		l.file.Close()
		f, err := os.Create(l.filePath)
		if err != nil {
			return err
		}
		l.file = f
	}
	return nil
}

// Close 关闭日志文件。
func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		l.file.Close()
	}
}

// Writer 返回一个 io.Writer 用于标准库集成。
func (l *Logger) Writer(level Level) io.Writer {
	return &logWriter{l: l, level: level}
}

type logWriter struct {
	l     *Logger
	level Level
}

func (w *logWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		w.l.log(w.level, "%s", msg)
	}
	return len(p), nil
}

func (l *Logger) rotateIfNeeded() {
	info, err := l.file.Stat()
	if err != nil || info.Size() < maxLogSize {
		return
	}

	l.file.Close()

	old := l.filePath + ".1"
	os.Remove(old)
	os.Rename(l.filePath, old)

	f, err := os.Create(l.filePath)
	if err == nil {
		l.file = f
	}
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	if err != nil {
		return false
	}
	return (info.Mode() & os.ModeCharDevice) != 0
}
