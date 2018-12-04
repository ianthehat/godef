package source

import "github.com/rogpeppe/godef/bench/target"

func BenchSource() {
	target.BenchTarget() //@godef("Target", BenchTarget)
}

