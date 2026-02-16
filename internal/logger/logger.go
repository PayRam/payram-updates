package logger

import (
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"

	"github.com/sirupsen/logrus"
)

var (
	base *logrus.Logger
	once sync.Once
)

// Init sets up the shared logger once with env-configured level.
func Init() {
	once.Do(func() {
		base = logrus.New()
		base.SetOutput(os.Stdout)
		base.SetFormatter(&logrus.TextFormatter{FullTimestamp: true})
		base.SetLevel(parseLevel(os.Getenv("LOG_LEVEL")))
	})
}

func parseLevel(raw string) logrus.Level {
	level := strings.TrimSpace(strings.ToLower(raw))
	if level == "" {
		return logrus.InfoLevel
	}
	parsed, err := logrus.ParseLevel(level)
	if err != nil {
		return logrus.InfoLevel
	}
	return parsed
}

func ensure() {
	if base == nil {
		Init()
	}
}

// StdLogger returns a stdlib logger that writes into the shared logrus logger.
func StdLogger() *log.Logger {
	ensure()
	return log.New(base.WriterLevel(logrus.InfoLevel), "", 0)
}

// Infof logs an informational message with class/method context.
func Infof(className, methodName, format string, args ...interface{}) {
	ensure()
	base.Infof("%s -> %s: %s", className, methodName, fmt.Sprintf(format, args...))
}

// Warnf logs a warning message with class/method context.
func Warnf(className, methodName, format string, args ...interface{}) {
	ensure()
	base.Warnf("%s -> %s: %s", className, methodName, fmt.Sprintf(format, args...))
}

// Error logs an error message with required format.
func Error(className, methodName string, err error) {
	ensure()
	if err == nil {
		err = errors.New("unknown error")
	}
	base.Errorf("%s -> %s: %s", className, methodName, err.Error())
}

// ErrorMsg logs a string as an error with required format.
func ErrorMsg(className, methodName, message string) {
	ensure()
	base.Errorf("%s -> %s: %s", className, methodName, message)
}
