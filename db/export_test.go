package db

// PruneBatch lets db_test lower the prune commit size.
var PruneBatch = &pruneBatch

// SetPreloadDirs lowers the dir count that makes listings read the whole dir table.
func SetPreloadDirs(n int) (restore func()) {
	old := preloadDirs
	preloadDirs = n
	return func() { preloadDirs = old }
}
