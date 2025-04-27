package log

import (
	"fmt"           // For error formatting and string building
	"os"            // For creating directories
	"path/filepath" // For OS-agnostic path joining

	"github.com/mattn/go-colorable"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

func Init_Logger(serviceName string, logDir string) *zap.Logger {
	if serviceName == "" {
		fmt.Fprintf(os.Stderr, "ERROR: Service name cannot be empty for logger initialization\n")
		return nil
	}
	if logDir == "" {
		logDir = "./logs" // Default log directory if not specified
	}

	// Ensure the log directory exists
	err := os.MkdirAll(logDir, 0755) // 0755 permissions: rwxr-xr-x
	if err != nil {
		// Use fmt.Fprintf to stderr for initialization errors as logger isn't ready
		fmt.Fprintf(os.Stderr, "ERROR: Failed to create log directory '%s': %v\n", logDir, err)
		return nil
	}

	// --- 1. Configure Console 输出 ---
	consoleEncoderConfig := zap.NewDevelopmentEncoderConfig()
	consoleEncoderConfig.EncodeLevel = colorLevelEncoder // Use custom color encoder
	consoleEncoder := zapcore.NewConsoleEncoder(consoleEncoderConfig)
	consoleSyncer := zapcore.AddSync(colorable.NewColorableStdout())
	consoleCore := zapcore.NewCore(consoleEncoder, consoleSyncer, zapcore.DebugLevel)

	// --- 2. 配置 File 输出 (using lumberjack for the specific service) ---
	logFilePath := filepath.Join(logDir, fmt.Sprintf("%s.log", serviceName))

	lumberjackLogger := &lumberjack.Logger{
		Filename:   logFilePath, // Service-specific log file path
		MaxSize:    100,         // Max size in MB before rotation
		MaxBackups: 10,          // Max number of old log files to keep
		MaxAge:     30,          // Max days to retain old log files
		Compress:   false,       // Compress rolled files (optional)
	}
	fileSyncer := zapcore.AddSync(lumberjackLogger)

	// File logs typically use JSON format and Info level or higher
	fileEncoderConfig := zap.NewProductionEncoderConfig()
	fileEncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	fileEncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder
	fileEncoder := zapcore.NewJSONEncoder(fileEncoderConfig)
	// File only logs Info level and above
	fileCore := zapcore.NewCore(fileEncoder, fileSyncer, zapcore.InfoLevel)

	// --- 3. 组合 Core (using zapcore.NewTee) ---
	core := zapcore.NewTee(
		consoleCore, // Log to console
		fileCore,    // Log to service-specific file
	)

	// --- 4. 构建 Logger ---
	// Create the base logger with caller info and error stacktraces
	baseLogger := zap.New(core, zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel))

	// --- 5. Add Service Name as a Default Field ---
	// Use With to create a logger that automatically adds the service name field
	logger := baseLogger.With(zap.String("service", serviceName))

	logger.Info("Logger initialized for service", zap.String("log_file", logFilePath))

	return logger
}

// colorLevelEncoder remains the same
func colorLevelEncoder(level zapcore.Level, enc zapcore.PrimitiveArrayEncoder) {
	// ... (keep the existing colorLevelEncoder implementation) ...
	switch level {
	case zapcore.DebugLevel:
		enc.AppendString("\x1b[35mDEBUG\x1b[0m") // 紫色
	case zapcore.InfoLevel:
		enc.AppendString("\x1b[32mINFO\x1b[0m") // 绿色
	case zapcore.WarnLevel:
		enc.AppendString("\x1b[33mWARN\x1b[0m") // 黄色
	case zapcore.ErrorLevel:
		enc.AppendString("\x1b[31mERROR\x1b[0m") // 红色
	case zapcore.DPanicLevel, zapcore.PanicLevel, zapcore.FatalLevel:
		enc.AppendString("\x1b[31m" + level.CapitalString() + "\x1b[0m")
	default:
		enc.AppendString("\x1b[36m" + level.CapitalString() + "\x1b[0m")
	}
}
