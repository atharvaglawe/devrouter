package otherpath

import "urlbuildersvc/otherpath/constant"

// A homonym GetPath implementor in an unrelated package. It returns a
// non-route constant, so it adds a competing candidate to the
// name-based getter resolution without producing a spurious edge.
type otherPath struct{}

func (o *otherPath) GetPath() string {
	return constant.OtherPath
}

func New() *otherPath { return &otherPath{} }
