// Copyright 2026 The Lodia Authors.
// SPDX-License-Identifier: Apache-2.0

// Command cluster-api-provider-lodia runs the LodiaControlPlane controller:
// one manager, leader-elected on request, whose cache holds no Secret data.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/spf13/pflag"
	cliflag "k8s.io/component-base/cli/flag"
	logsv1 "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/logs/json/register"
	"k8s.io/klog/v2"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/cluster-api/util/flags"
	ctrl "sigs.k8s.io/controller-runtime"

	"go.lodia.io/cluster-api-provider-lodia/internal/controller"
)

var (
	config     controller.Config
	logOptions = logsv1.NewLoggingConfiguration()
)

// InitFlags registers the flags of Cluster API's kubeadm control plane
// provider that apply here, under its names and with its defaults, but for the
// concurrency: --lodiacontrolplane-concurrency defaults to 10, where its
// --kubeadmcontrolplane-concurrency defaults to 100:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/controlplane/kubeadm/main.go#L128-L217
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/controlplane/kubeadm/main.go#L160-L161
func InitFlags(fs *pflag.FlagSet) {
	logsv1.AddFlags(logOptions, fs)

	fs.BoolVar(&config.LeaderElect, "leader-elect", false,
		"Enable leader election for controller manager. Enabling this will ensure there is only one active controller manager.")

	fs.DurationVar(&config.LeaseDuration, "leader-elect-lease-duration", 1*time.Minute,
		"Interval at which non-leader candidates will wait to force acquire leadership (duration string)")

	fs.DurationVar(&config.RenewDeadline, "leader-elect-renew-deadline", 40*time.Second,
		"Duration that the leading controller manager will retry refreshing leadership before giving up (duration string)")

	fs.DurationVar(&config.RetryPeriod, "leader-elect-retry-period", 5*time.Second,
		"Duration the LeaderElector clients should wait between tries of actions (duration string)")

	fs.StringVar(&config.Namespace, "namespace", "",
		"Namespace that the controller watches to reconcile cluster-api objects. If unspecified, the controller watches for cluster-api objects across all namespaces.")

	fs.StringVar(&config.WatchFilterValue, "watch-filter", "",
		fmt.Sprintf("Label value that the controller watches to reconcile cluster-api objects. Label key is always %s. If unspecified, the controller watches for all cluster-api objects.", clusterv1.WatchLabel))

	fs.StringVar(&config.ProfilerAddress, "profiler-address", "",
		"Bind address to expose the pprof profiler (e.g. localhost:6060)")

	fs.IntVar(&config.Concurrency, "lodiacontrolplane-concurrency", 10,
		"Number of LodiaControlPlanes to process simultaneously")

	fs.DurationVar(&config.SyncPeriod, "sync-period", 10*time.Minute,
		"The minimum interval at which watched resources are reconciled (e.g. 15m)")

	fs.StringVar(&config.HealthAddr, "health-addr", ":9440",
		"The address the health endpoint binds to.")

	flags.AddManagerOptions(fs, &config.Diagnostics)
}

// prepare readies fs as Cluster API's kubeadm control plane provider's main
// does: InitFlags, '_' read as '-', Go's flag set for --kubeconfig, and --v=2
// unless given:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/controlplane/kubeadm/main.go#L229-L237
func prepare(fs *pflag.FlagSet) error {
	InitFlags(fs)
	fs.SetNormalizeFunc(cliflag.WordSepNormalizeFunc)
	fs.AddGoFlagSet(flag.CommandLine)
	return fs.Set("v", "2")
}

// load parses args into fs, readied by prepare, and applies the logging
// configuration, as Cluster API's kubeadm control plane provider's main does:
// https://github.com/kubernetes-sigs/cluster-api/blob/v1.15.0-beta.0/controlplane/kubeadm/main.go#L238-L250
func load(fs *pflag.FlagSet, args []string) error {
	if err := prepare(fs); err != nil {
		return fmt.Errorf("set the default log level: %w", err)
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := logsv1.ValidateAndApply(logOptions, nil); err != nil {
		return err
	}
	ctrl.SetLogger(klog.Background())
	return nil
}

func main() {
	if err := load(pflag.CommandLine, os.Args[1:]); err != nil {
		fmt.Printf("Unable to start manager: %v\n", err)
		os.Exit(1)
	}
	log := ctrl.Log.WithName("setup")

	mgr, err := controller.NewManager(ctrl.GetConfigOrDie(), config)
	if err != nil {
		log.Error(err, "build the manager")
		os.Exit(1)
	}

	log.Info("starting the manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "run the manager")
		os.Exit(1)
	}
}
