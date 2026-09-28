package transport

// WithSPDYOnly makes the kube-exec Transports of a Factory use SPDY without
// trying WebSocket first, so that a test runs the same behaviour over the
// protocol the fallback would take. It has to follow WithKubeExec.
func WithSPDYOnly() FactoryOption {
	return func(f *factory) {
		if f.kube != nil {
			f.kube.spdyOnly = true
		}
	}
}
