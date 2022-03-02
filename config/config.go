package config

// Ops represents the commandline/environment options for the program
type Ops struct {
	LogLevel            string `long:"log-level" env:"LOG_LEVEL" description:"Log level" default:"info"`
	NodeTaint           string `long:"node-taint" env:"NODE_TAINT" description:"The node taints that's going to remove."`
	DaemonSetAnnotation string `long:"daemonset-annotation" env:"DAEMONSET_ANNOTATION" description:"The annotation of the daemonset to watch, key:value pairs, comma delemited"`
	BindAddr            string `long:"bind-address" short:"p" env:"BIND_ADDRESS" default:":9797" description:"address for binding metrics listener"`
	DaemonSetLabel      string `long:"daemonset-label" env:"DAEMONSET_LABEL" description:"The label of the daemonset to watch, key:value pairs, comma delemited"`
}
