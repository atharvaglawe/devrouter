package wrapper

type DataStore struct{}

func New(v string) *DataStore {
	return &DataStore{}
}
