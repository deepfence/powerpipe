package cmdconfig

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/hashicorp/go-hclog"
	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/turbot/go-kit/helpers"
	"github.com/turbot/go-kit/logging"
	"github.com/turbot/powerpipe/internal/dashboard"
	sdklogging "github.com/turbot/steampipe-plugin-sdk/v5/logging"
	"github.com/turbot/steampipe-plugin-sdk/v5/plugin"
	"github.com/turbot/steampipe-plugin-sdk/v5/sperr"
	"github.com/turbot/steampipe/pkg/cloud"
	"github.com/turbot/steampipe/pkg/constants"
	"github.com/turbot/steampipe/pkg/constants/runtime"
	"github.com/turbot/steampipe/pkg/error_helpers"
	"github.com/turbot/steampipe/pkg/filepaths"
	"github.com/turbot/steampipe/pkg/steampipeconfig"
	"github.com/turbot/steampipe/pkg/task"
	"github.com/turbot/steampipe/pkg/utils"
	"github.com/turbot/steampipe/pkg/version"
)

var waitForTasksChannel chan struct{}
var tasksCancelFn context.CancelFunc

// postRunHook is a function that is executed after the PostRun of every command handler
func postRunHook(cmd *cobra.Command, args []string) {
	utils.LogTime("cmdhook.postRunHook start")
	defer utils.LogTime("cmdhook.postRunHook end")

	if waitForTasksChannel != nil {
		// wait for the async tasks to finish
		select {
		case <-time.After(100 * time.Millisecond):
			tasksCancelFn()
			return
		case <-waitForTasksChannel:
			return
		}
	}
}

// postRunHook is a function that is executed before the PreRun of every command handler
func preRunHook(cmd *cobra.Command, args []string) {
	utils.LogTime("cmdhook.preRunHook start")
	defer utils.LogTime("cmdhook.preRunHook end")

	viper.Set(constants.ConfigKeyActiveCommand, cmd)
	viper.Set(constants.ConfigKeyActiveCommandArgs, args)
	viper.Set(constants.ConfigKeyIsTerminalTTY, isatty.IsTerminal(os.Stdout.Fd()))

	// steampipe completion should not create INSTALL DIR or seup/init global config
	if cmd.Name() == "completion" {
		return
	}

	// create a buffer which can be used as a sink for log writes
	// till INSTALL_DIR is setup in initGlobalConfig
	logBuffer := bytes.NewBuffer([]byte{})

	// create a logger before initGlobalConfig - we may need to reinitialize the logger
	// depending on the value of the log_level value in global general options
	createLogger(logBuffer, cmd)

	// set up the global viper config with default values from
	// config files and ENV variables
	ew := initGlobalConfig()
	// display any warnings
	ew.ShowWarnings()
	// check for error
	error_helpers.FailOnError(ew.Error)

	// if the log level was set in the general config
	if logLevelNeedsReset() {
		logLevel := viper.GetString(constants.ArgLogLevel)
		// set my environment to the desired log level
		// so that this gets inherited by any other process
		// started by this process (postgres/plugin-manager)
		error_helpers.FailOnErrorWithMessage(
			os.Setenv(sdklogging.EnvLogLevel, logLevel),
			"Failed to setup logging",
		)
	}

	// recreate the logger
	// this will put the new log level (if any) to effect as well as start streaming to the
	// log file.
	createLogger(logBuffer, cmd)

	// runScheduledTasks skips running tasks if this instance is the plugin manager
	waitForTasksChannel = runScheduledTasks(cmd.Context(), cmd, args, ew)

	// set the max memory if specified
	setMemoryLimit()
}

func setMemoryLimit() {
	maxMemoryBytes := viper.GetInt64(constants.ArgMemoryMaxMb) * 1024 * 1024
	if maxMemoryBytes > 0 {
		// set the max memory
		debug.SetMemoryLimit(maxMemoryBytes)
	}
}

// runScheduledTasks runs the task runner and returns a channel which is closed when
// task run is complete
//
// runScheduledTasks skips running tasks if this instance is the plugin manager
func runScheduledTasks(ctx context.Context, cmd *cobra.Command, args []string, ew *error_helpers.ErrorAndWarnings) chan struct{} {
	// skip running the task runner if this is the plugin manager
	// since it's supposed to be a daemon
	if task.IsPluginManagerCmd(cmd) {
		return nil
	}

	taskUpdateCtx, cancelFn := context.WithCancel(ctx)
	tasksCancelFn = cancelFn

	return task.RunTasks(
		taskUpdateCtx,
		cmd,
		args,
		// pass the config value in rather than runRasks querying viper directly - to avoid concurrent map access issues
		// (we can use the update-check viper config here, since initGlobalConfig has already set it up
		// with values from the config files and ENV settings - update-check cannot be set from the command line)
		task.WithUpdateCheck(viper.GetBool(constants.ArgUpdateCheck)),
		// show deprecation warnings
		task.WithPreHook(func(_ context.Context) {
			displayDeprecationWarnings(ew)
		}),
	)

}

// the log level will need resetting if
//
//	this process does not have a log level set in it's environment
//	the GlobalConfig has a loglevel set
func logLevelNeedsReset() bool {
	envLogLevelIsSet := envLogLevelSet()
	generalOptionsSet := (steampipeconfig.GlobalConfig.GeneralOptions != nil && steampipeconfig.GlobalConfig.GeneralOptions.LogLevel != nil)

	return !envLogLevelIsSet && generalOptionsSet
}

// envLogLevelSet checks whether any of the current or legacy log level env vars are set
func envLogLevelSet() bool {
	_, ok := os.LookupEnv(sdklogging.EnvLogLevel)
	if ok {
		return ok
	}
	// handle legacy env vars
	for _, e := range sdklogging.LegacyLogLevelEnvVars {
		_, ok = os.LookupEnv(e)
		if ok {
			return ok
		}
	}
	return false
}

// initConfig reads in config file and ENV variables if set.
func initGlobalConfig() *error_helpers.ErrorAndWarnings {
	utils.LogTime("cmdconfig.initGlobalConfig start")
	defer utils.LogTime("cmdconfig.initGlobalConfig end")

	var cmd = viper.Get(constants.ConfigKeyActiveCommand).(*cobra.Command)
	// set-up viper with defaults from the env and default workspace profile
	err := bootstrapViper(cmd)
	if err != nil {
		return error_helpers.NewErrorsAndWarning(err)
	}

	// set global containing the configured install dir (create directory if needed)
	ensureInstallDir(viper.GetString(constants.ArgInstallDir))

	// load the connection config and HCL options
	config, loadConfigErrorsAndWarnings := steampipeconfig.LoadSteampipeConfig(viper.GetString(constants.ArgModLocation), cmd.Name())
	if loadConfigErrorsAndWarnings.Error != nil {
		return loadConfigErrorsAndWarnings
	}

	// store global config
	steampipeconfig.GlobalConfig = config

	// set viper defaults from the loaded config
	SetDefaultsFromConfig(steampipeconfig.GlobalConfig.ConfigMap())

	// set the rest of the defaults from ENV
	// ENV takes precedence over any default configuration
	setDefaultsFromEnv()

	// NOTE: we need to resolve the token separately
	// - that is because we need the resolved value of ArgCloudHost in order to load any saved token
	// and we cannot get this until the other config has been resolved
	err = setCloudTokenDefault()
	if err != nil {
		loadConfigErrorsAndWarnings.Error = err
		return loadConfigErrorsAndWarnings
	}

	// now validate all config values have appropriate values
	ew := validateConfig()
	error_helpers.FailOnErrorWithMessage(ew.Error, "failed to validate config")

	loadConfigErrorsAndWarnings.Merge(ew)

	return loadConfigErrorsAndWarnings
}

func setCloudTokenDefault() error {
	/*
	   saved cloud token
	   cloud_token in default workspace
	   explicit env var (STEAMIPE_CLOUD_TOKEN ) wins over
	   cloud_token in specific workspace
	*/
	// set viper defaults in order of increasing precedence
	// 1) saved cloud token
	savedToken, err := cloud.LoadToken()
	if err != nil {
		return err
	}
	if savedToken != "" {
		viper.SetDefault(constants.ArgCloudToken, savedToken)
	}
	// 3) env var (STEAMIPE_CLOUD_TOKEN )
	SetDefaultFromEnv(constants.EnvCloudToken, constants.ArgCloudToken, String)

	return nil
}

// now validate  config values have appropriate values
// (currently validates telemetry)
func validateConfig() *error_helpers.ErrorAndWarnings {
	var res = &error_helpers.ErrorAndWarnings{}
	telemetry := viper.GetString(constants.ArgTelemetry)
	if !helpers.StringSliceContains(constants.TelemetryLevels, telemetry) {
		res.Error = sperr.New(`invalid value of 'telemetry' (%s), must be one of: %s`, telemetry, strings.Join(constants.TelemetryLevels, ", "))
		return res
	}
	if _, legacyDiagnosticsSet := os.LookupEnv(plugin.EnvLegacyDiagnosticsLevel); legacyDiagnosticsSet {
		res.AddWarning(fmt.Sprintf("Environment variable %s is deprecated - use %s", plugin.EnvLegacyDiagnosticsLevel, plugin.EnvDiagnosticsLevel))
	}
	res.Error = plugin.ValidateDiagnosticsEnvVar()

	return res
}

// create a hclog logger with the level specified by the SP_LOG env var
func createLogger(logBuffer *bytes.Buffer, cmd *cobra.Command) {
	if task.IsPluginManagerCmd(cmd) {
		// nothing to do here - plugin manager sets up it's own logger
		// refer https://github.com/turbot/steampipe/blob/710a96d45fd77294de8d63d77bf78db65133e5ca/cmd/plugin_manager.go#L102
		return
	}

	level := sdklogging.LogLevel()
	var logDestination io.Writer
	if len(filepaths.SteampipeDir) == 0 {
		// write to the buffer - this is to make sure that we don't lose logs
		// till the time we get the log directory
		logDestination = logBuffer
	} else {
		logDestination = logging.NewRotatingLogWriter(filepaths.EnsureLogDir(), "steampipe")

		// write out the buffered contents
		_, _ = logDestination.Write(logBuffer.Bytes())
	}

	hcLevel := hclog.LevelFromString(level)

	options := &hclog.LoggerOptions{
		// make the name unique so that logs from this instance can be filtered
		Name:       fmt.Sprintf("steampipe [%s]", runtime.ExecutionID),
		Level:      hcLevel,
		Output:     logDestination,
		TimeFn:     func() time.Time { return time.Now().UTC() },
		TimeFormat: "2006-01-02 15:04:05.000 UTC",
	}
	logger := sdklogging.NewLogger(options)
	log.SetOutput(logger.StandardWriter(&hclog.StandardLoggerOptions{InferLevels: true}))
	log.SetPrefix("")
	log.SetFlags(0)

	// if the buffer is empty then this is the first time the logger is getting setup
	// write out a banner
	if logBuffer.Len() == 0 {
		// pump in the initial set of logs
		// this will also write out the Execution ID - enabling easy filtering of logs for a single execution
		// we need to do this since all instances will log to a single file and logs will be interleaved
		log.Printf("[INFO] ********************************************************\n")
		log.Printf("[INFO] **%16s%20s%16s**\n", " ", fmt.Sprintf("Steampipe [%s]", runtime.ExecutionID), " ")
		log.Printf("[INFO] ********************************************************\n")
		log.Printf("[INFO] Version:   v%s\n", version.VersionString)
		log.Printf("[INFO] Log level: %s\n", sdklogging.LogLevel())
		log.Printf("[INFO] Log date: %s\n", time.Now().Format("2006-01-02"))
		//
	}
}

func ensureInstallDir(installDir string) {
	log.Printf("[TRACE] ensureInstallDir %s", installDir)
	if _, err := os.Stat(installDir); os.IsNotExist(err) {
		log.Printf("[TRACE] creating install dir")
		err = os.MkdirAll(installDir, 0755)
		error_helpers.FailOnErrorWithMessage(err, fmt.Sprintf("could not create installation directory: %s", installDir))
	}

	// store as PowerpipeDir
	dashboard.PowerpipeDir = installDir

	// also set the global containing the configured install dir
	// this is a hack since we are using LoadConfig from the steampipe repository
	// and it needs access to this value
	filepaths.SteampipeDir = installDir
}

// displayDeprecationWarnings shows the deprecated warnings in a formatted way
func displayDeprecationWarnings(errorsAndWarnings *error_helpers.ErrorAndWarnings) {
	if len(errorsAndWarnings.Warnings) > 0 {
		fmt.Println(color.YellowString(fmt.Sprintf("\nDeprecation %s:", utils.Pluralize("warning", len(errorsAndWarnings.Warnings)))))
		for _, warning := range errorsAndWarnings.Warnings {
			fmt.Printf("%s\n\n", warning)
		}
		fmt.Println("For more details, see https://steampipe.io/docs/reference/config-files/workspace")
		fmt.Println()
	}
}
