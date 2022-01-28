package logger

import logrus "github.com/sirupsen/logrus"

// Logger represent common interface for logging function
type Logger interface {
	Errorf(format string, args ...interface{})
	Error(args ...interface{})
	Fatalf(format string, args ...interface{})
	Fatal(args ...interface{})
	Infof(format string, args ...interface{})
	Info(args ...interface{})
	Warnf(format string, args ...interface{})
	Warn(args ...interface{})
	Debugf(format string, args ...interface{})
	Debug(args ...interface{})
}

type LoggerWithOutput interface {
	Logger
	Output(args ...interface{})
}

type LogrousWithOutput struct {
	*logrus.Logger
}

func (l *LogrousWithOutput) Output(args ...interface{}) {
	l.Println(args)
}

type LeveledLogger interface {
	Error(msg string, keysAndValues ...interface{})
	Info(msg string, keysAndValues ...interface{})
	Debug(msg string, keysAndValues ...interface{})
	Warn(msg string, keysAndValues ...interface{})
}

type LeveledLogrus struct {
	*logrus.Logger
}

func (l *LeveledLogrus) fields(keysAndValues ...interface{}) map[string]interface{} {
	fields := make(map[string]interface{})

	for i := 0; i < len(keysAndValues)-1; i += 2 {
		fields[keysAndValues[i].(string)] = keysAndValues[i+1]
	}

	return fields
}

func (l *LeveledLogrus) Error(msg string, keysAndValues ...interface{}) {
	l.WithFields(l.fields(keysAndValues...)).Error(msg)
}

func (l *LeveledLogrus) Info(msg string, keysAndValues ...interface{}) {
	l.WithFields(l.fields(keysAndValues...)).Info(msg)
}
func (l *LeveledLogrus) Debug(msg string, keysAndValues ...interface{}) {
	l.WithFields(l.fields(keysAndValues...)).Debug(msg)
}

func (l *LeveledLogrus) Warn(msg string, keysAndValues ...interface{}) {
	l.WithFields(l.fields(keysAndValues...)).Warn(msg)
}
