package runner

// DBPath 返回数据库文件路径(升级回滚时还原备份要用)。
func (r *Runner) DBPath() string { return r.dbPath }
