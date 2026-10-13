package core

import "time"

func unixMillis() int64 { return time.Now().UnixMilli() }
