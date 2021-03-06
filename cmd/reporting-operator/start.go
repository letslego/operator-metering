package main

import (
	goflag "flag"
	"fmt"
	"io/ioutil"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/operator-framework/operator-metering/pkg/operator"
)

var (
	defaultHiveHost      = "hive:10000"
	defaultPrestoHost    = "presto:8080"
	defaultPromHost      = "http://prometheus-k8s.monitoring.svc:9090/"
	defaultLeaseDuration = time.Second * 60
	// cfg is the config for our operator
	cfg operator.Config

	logLevelStr         string
	logFullTimestamp    bool
	logDisableTimestamp bool
)

var rootCmd = &cobra.Command{
	Use:   "metering",
	Short: "",
	RunE: func(cmd *cobra.Command, _ []string) error {
		return cmd.Help()
	},
}

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "starts the Metering operator",
	Run:   startReporting,
}

func AddCommands() {
	rootCmd.AddCommand(startCmd)
}

func init() {
	// globally set time to UTC
	time.Local = time.UTC

	// initialize the pointers before we assign into them below
	cfg.PrometheusQueryConfig.QueryInterval = new(meta.Duration)
	cfg.PrometheusQueryConfig.StepSize = new(meta.Duration)
	cfg.PrometheusQueryConfig.ChunkSize = new(meta.Duration)

	startCmd.Flags().StringVar(&logLevelStr, "log-level", log.DebugLevel.String(), "log level")
	startCmd.Flags().BoolVar(&logFullTimestamp, "log-timestamp", true, "log full timestamp if true, otherwise log time since startup")
	startCmd.Flags().BoolVar(&logDisableTimestamp, "disable-timestamp", false, "disable timestamp logging")

	startCmd.Flags().StringVar(&cfg.Kubeconfig, "kubeconfig", "", "use kubeconfig provided instead of detecting defaults")
	startCmd.Flags().StringVar(&cfg.Namespace, "namespace", "", "namespace the operator is running in")
	startCmd.Flags().StringVar(&cfg.HiveHost, "hive-host", defaultHiveHost, "the hostname:port for connecting to Hive")
	startCmd.Flags().StringVar(&cfg.PrestoHost, "presto-host", defaultPrestoHost, "the hostname:port for connecting to Presto")
	startCmd.Flags().StringVar(&cfg.PrometheusConfig.Address, "prometheus-host", defaultPromHost, "the URL string for connecting to Prometheus")
	startCmd.Flags().BoolVar(&cfg.PrometheusConfig.SkipTLSVerify, "prometheus-skip-tls-verify", false, "Skip TLS verification")
	startCmd.Flags().StringVar(&cfg.PrometheusConfig.BearerToken, "prometheus-bearer-token", "", "Bearer token to authenticate against Prometheus.")

	startCmd.Flags().BoolVar(&cfg.DisablePromsum, "disable-promsum", false, "disables collecting Prometheus metrics periodically")
	startCmd.Flags().BoolVar(&cfg.LogDMLQueries, "log-dml-queries", false, "logDMLQueries controls if we log data manipulation queries made via Presto (SELECT, INSERT, etc)")
	startCmd.Flags().BoolVar(&cfg.LogDDLQueries, "log-ddl-queries", false, "logDDLQueries controls if we log data definition language queries made via Hive (CREATE TABLE, DROP TABLE, etc)")
	startCmd.Flags().BoolVar(&cfg.EnableFinalizers, "enable-finalizers", false, "If enabled, then finalizers will be set on some resources to ensure the reporting-operator is able to perform cleanup before the resource is deleted from the API")

	startCmd.Flags().DurationVar(&cfg.PrometheusQueryConfig.QueryInterval.Duration, "promsum-interval", operator.DefaultPrometheusQueryInterval, "controls how often the operator polls Prometheus for metrics")
	startCmd.Flags().DurationVar(&cfg.PrometheusQueryConfig.StepSize.Duration, "promsum-step-size", operator.DefaultPrometheusQueryStepSize, "the query step size for Promethus query. This controls resolution of results")
	startCmd.Flags().DurationVar(&cfg.PrometheusQueryConfig.ChunkSize.Duration, "promsum-chunk-size", operator.DefaultPrometheusQueryChunkSize, "controls how much the range query window sizeby limiting the range query to a range of time no longer than this duration")
	startCmd.Flags().DurationVar(&cfg.LeaderLeaseDuration, "lease-duration", defaultLeaseDuration, "controls how much time elapses before declaring leader")

	startCmd.Flags().BoolVar(&cfg.APITLSConfig.UseTLS, "use-tls", false, "If true, uses TLS to secure HTTP API traffix")
	startCmd.Flags().StringVar(&cfg.APITLSConfig.TLSCert, "tls-cert", "", "If use-tls is true, specifies the path to the TLS certificate.")
	startCmd.Flags().StringVar(&cfg.APITLSConfig.TLSKey, "tls-key", "", "If use-tls is true, specifies the path to the TLS private key.")

	startCmd.Flags().BoolVar(&cfg.MetricsTLSConfig.UseTLS, "metrics-use-tls", false, "If true, uses TLS to secure Prometheus Metrics endpoint traffix")
	startCmd.Flags().StringVar(&cfg.MetricsTLSConfig.TLSCert, "metrics-tls-cert", "", "If metrics-use-tls is true, specifies the path to the TLS certificate to use for the Metrics endpoint.")
	startCmd.Flags().StringVar(&cfg.MetricsTLSConfig.TLSKey, "metrics-tls-key", "", "If metrics-use-tls is true, specifies the path to the TLS private key to use for the Metrics endpoint.")
}

func main() {
	// fix https://github.com/kubernetes/kubernetes/issues/17162
	goflag.CommandLine.Set("logtostderr", "true")
	goflag.CommandLine.Parse(nil)
	log.SetFormatter(&log.TextFormatter{
		FullTimestamp:    logFullTimestamp,
		DisableTimestamp: logDisableTimestamp,
	})

	AddCommands()

	rootCmd.ParseFlags(os.Args[1:])

	if err := SetFlagsFromEnv(startCmd.Flags(), "REPORTING_OPERATOR"); err != nil {
		log.WithError(err).Fatalf("error setting flags from environment variables: %v", err)
	}
	if err := rootCmd.Execute(); err != nil {
		log.WithError(err).Fatalf("error executing command: %v", err)
	}
}

func startReporting(cmd *cobra.Command, args []string) {
	logger := newLogger()
	if cfg.Namespace == "" {
		namespace, err := ioutil.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace")
		if err != nil {
			logger.WithError(err).Fatal("could not determine namespace")
		}
		cfg.Namespace = string(namespace)
	}

	cfg.PodName = os.Getenv("POD_NAME")

	var err error
	cfg.Hostname, err = os.Hostname()
	if err != nil {
		logger.Fatalf("unable to get hostname, err: %s", err)
	}

	signalStopCh := setupSignals()
	runReporting(logger, cfg, signalStopCh)
}

func runReporting(logger log.FieldLogger, cfg operator.Config, stopCh <-chan struct{}) {
	op, err := operator.New(logger, cfg)
	if err != nil {
		logger.WithError(err).Fatal("unable to setup reporting-operator")
	}
	if err = op.Run(stopCh); err != nil {
		logger.WithError(err).Fatal("error occurred while the reporting-operator was running")
	}
	logger.Infof("reporting-operator has stopped")
}

// SetFlagsFromEnv parses all registered flags in the given flagset,
// and if they are not already set it attempts to set their values from
// environment variables. Environment variables take the name of the flag but
// are UPPERCASE, and any dashes are replaced by underscores. Environment
// variables additionally are prefixed by the given string followed by
// and underscore. For example, if prefix=PREFIX: some-flag => PREFIX_SOME_FLAG
func SetFlagsFromEnv(fs *pflag.FlagSet, prefix string) (err error) {
	alreadySet := make(map[string]bool)
	fs.Visit(func(f *pflag.Flag) {
		alreadySet[f.Name] = true
	})
	fs.VisitAll(func(f *pflag.Flag) {
		if !alreadySet[f.Name] {
			key := prefix + "_" + strings.ToUpper(strings.Replace(f.Name, "-", "_", -1))
			val := os.Getenv(key)
			if val != "" {
				if serr := fs.Set(f.Name, val); serr != nil {
					err = fmt.Errorf("invalid value %q for %s: %v", val, key, serr)
				}
			}
		}
	})
	return err
}

func setupSignals() chan struct{} {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	stopCh := make(chan struct{})
	go func() {
		sig := <-sigs
		log.Infof("got signal %s, performing shutdown", sig)
		close(stopCh)
	}()
	return stopCh
}

func newLogger() log.FieldLogger {
	logger := log.WithFields(log.Fields{
		"app": "metering",
	})
	logLevel, err := log.ParseLevel(logLevelStr)
	if err != nil {
		logger.WithError(err).Fatalf("invalid log level: %s", logLevelStr)
	}
	logger.Infof("setting log level to %s", logLevel.String())
	logger.Logger.Level = logLevel
	return logger

}
