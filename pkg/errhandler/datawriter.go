package errhandler

import (
	"os"

	"github.com/vesoft-inc/nebula-importer/v3/pkg/base"
)

type DataWriter interface {
	Init(*os.File)
	Write([]base.Data)
	Flush()
	Error() error
}
