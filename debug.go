// +build debug

package cu

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

var tc uint32

const DEBUG = true

var _logger_ = log.New(os.Stderr, "", 0)
var replacement = "\n"

func tabcount() int {
	return int(atomic.LoadUint32(&tc))
}

func enterLoggingContext() {
	atomic.AddUint32(&tc, 1)
	tabcount := tabcount()
	_logger_.SetPrefix(strings.Repeat("\t", tabcount))
	replacement = "\n" + strings.Repeat("\t", tabcount)
}

func leaveLoggingContext() {
	tabcount := tabcount()
	tabcount--

	if tabcount < 0 {
		atomic.StoreUint32(&tc, 0)
		tabcount = 0
	} else {
		atomic.StoreUint32(&tc, uint32(tabcount))
	}
	_logger_.SetPrefix(strings.Repeat("\t", tabcount))
	replacement = "\n" + strings.Repeat("\t", tabcount)
}

func logf(format string, others ...interface{}) {
	if DEBUG {
		// format = strings.Replace(format, "\n", replacement, -1)
		s := fmt.Sprintf(format, others...)
		s = strings.Replace(s, "\n", replacement, -1)
		_logger_.Println(s)
		// _logger_.Printf(format, others...)
	}
}

/* Debugging Utility Methods */

// introspect is useful for finding out what calls are going to be made in the batched call
func (ctx *BatchedContext) introspect() string {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "Queue: %d", len(ctx.queue))
	for _, v := range ctx.queue {
		fmt.Fprintf(&buf, "\n\t[QUEUE] %s", v.fnargs)
	}
	return buf.String()
}
