package contract

// IPathSelector is the interface field behind which the concrete
// path implementation hides. The resolver reaches the concrete
// defaultPath.GetPath via getter-name recursion.
type IPathSelector interface {
	GetPath() string
}
