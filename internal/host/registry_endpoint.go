package host

import (
	"net"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

// RegistryListenEnv is the managed worker environment variable that carries
// the node's actual loopback registry endpoint.
const RegistryListenEnv = "EREGION_REGISTRY_LISTEN"

// ManagedRegistryEndpoint reads the registry endpoint the host composition
// wrote into the worker's managed environment. There is deliberately no
// fallback to host intent or a built-in address: beta startup must fail
// explicitly when the managed setting is absent or not a loopback endpoint.
func ManagedRegistryEndpoint(getenv func(string) string) (string, error) {
	value := strings.TrimSpace(getenv(RegistryListenEnv))
	if value == "" {
		return "", contract.NewFault(contract.PreconditionFailed,
			"the managed registry endpoint "+RegistryListenEnv+" is missing",
			"Start the worker with EREGION_REGISTRY_LISTEN set by the node's managed host configuration; builds do not guess a registry endpoint.")
	}
	endpointHost, endpointPort, err := net.SplitHostPort(value)
	if err != nil || endpointHost == "" || endpointPort == "" {
		return "", contract.NewFault(contract.PreconditionFailed,
			"the managed registry endpoint is invalid",
			"EREGION_REGISTRY_LISTEN must be a loopback host:port such as 127.0.0.1:5000.")
	}
	port, portErr := strconv.Atoi(endpointPort)
	if portErr != nil || port < 1 || port > 65535 {
		return "", contract.NewFault(contract.PreconditionFailed,
			"the managed registry endpoint is invalid",
			"EREGION_REGISTRY_LISTEN must be a loopback host:port with a port from 1 to 65535.")
	}
	ip := net.ParseIP(endpointHost)
	if ip == nil || !ip.IsLoopback() {
		return "", contract.NewFault(contract.PreconditionFailed,
			"the managed registry endpoint is not loopback",
			"EREGION_REGISTRY_LISTEN must be a loopback host:port; a build never publishes to a remote address.")
	}
	return value, nil
}
