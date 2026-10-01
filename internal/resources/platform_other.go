//go:build !windows

package resources

// Off Windows (macOS, where homebase is developed) there's no implementation:
// the sampler returns at once and the dashboard hides the panel.
type unsupported struct{}

func newPlatform() platform { return unsupported{} }

func (unsupported) system() (cpuTimes, memInfo, error) { return cpuTimes{}, memInfo{}, ErrUnsupported }
func (unsupported) processes() ([]procLink, error)     { return nil, ErrUnsupported }
func (unsupported) stats(uint32) (procStats, bool)     { return procStats{}, false }
