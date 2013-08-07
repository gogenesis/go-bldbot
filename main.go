package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

var g_slaves = flag.String("slaves", "slaves.txt", "(JSON) file containing the list of slaves")
var g_parallel = flag.Bool("parallel", true, "run the build-slaves in parallel")

type Slave struct {
	Addr string // slave SSH address
	Name string // informative name of that slave
	Root string // path under which all build files and artifacts are stored
}

func (s *Slave) LocalCommandFileName() string {
	return filepath.Join(s.Name, "build.sh")
}

func (s *Slave) RemoteCommandFileName() string {
	return filepath.Join(s.Root, "build.sh")
}

type BuildReport struct {
	slave Slave
	msg   string
	err   error
}

type Builder struct {
	slave Slave
	w     *os.File // logfile
}

func (b Builder) run() BuildReport {
	fmt.Fprintf(b.w, "## build -- start [%v]\n", time.Now())
	fname := b.slave.CommandFileName()
	f, err := os.Open(fname)
	if err != nil {
		log.Printf(
			"no such file [%s] for slave [%s] (%v)\n",
			fname, b.slave.Addr, err,
		)
		return BuildReport{
			b.slave,
			fmt.Sprintf("no such file [%s] (err=%v)", fname, err),
			err,
		}
	}
	defer f.Close()

	{
		ssh := exec.Command(
			"ssh",
			b.slave.Addr,
			fmt.Sprintf("mkdir -p %s", b.slave.Root),
		)
		ssh.Stdout = b.w
		ssh.Stderr = b.w
		err = ssh.Run()
		if err != nil {
			// log.Printf("failed to copy [%s] to slave [%s] (err=%v)\ncmd=%v\n",
			// 	fname, b.slave.Name, err, ssh.Args,
			// )
			return BuildReport{
				b.slave,
				"failed to copy [" + fname + "]",
				err,
			}
		}
	}

	ssh := exec.Command(
		"scp", fname,
		fmt.Sprintf("%s:%s", b.slave.Addr, b.CommandFileName()),
	)

	fmt.Fprintf(b.w, "## build -- copying build-script...\n")
	b.w.Sync()
	ssh.Stdout = b.w
	ssh.Stderr = b.w
	err = ssh.Run()
	if err != nil {
		// log.Printf("failed to copy [%s] to slave [%s] (err=%v)\ncmd=%v\n",
		// 	fname, b.slave.Name, err, ssh.Args,
		// )
		return BuildReport{
			b.slave,
			"failed to copy [" + fname + "]",
			err,
		}
	}

	ssh = exec.Command(
		"ssh",
		b.slave.Addr,
		"time /build/build.sh",
	)
	fmt.Fprintf(b.w, "## build -- running build-script...\n")
	b.w.Sync()
	ssh.Stdout = b.w
	ssh.Stderr = b.w
	err = ssh.Run()
	if err != nil {
		// log.Printf("build failed for slave [%s] (err=%v)\n",
		// 	b.slave.Name, err,
		// )
		return BuildReport{
			b.slave,
			"build failed",
			err,
		}
	}

	// retrieve output
	ssh = exec.Command(
		"scp",
		b.slave.Addr+":/build/output/*.tar.gz", // */ dumb emacs
		"output/.",
	)
	fmt.Fprintf(b.w, "## build -- retrieving output(s)...\n")
	b.w.Sync()
	ssh.Stdout = b.w
	ssh.Stderr = b.w
	err = ssh.Run()
	b.w.Sync()
	b.w.Close()

	if err != nil {
		return BuildReport{
			b.slave,
			"failed to retrieve outputs",
			err,
		}
	}
	return BuildReport{b.slave, "ok", nil}
}

func main() {
	fmt.Printf(">>>\n>>> buildbot <<<\n>>>\n")
	flag.Parse()

	slaves := make([]Slave, 0, 2)
	f, err := os.Open(*g_slaves)
	if err != nil {
		log.Panicf("buildbot: could not open file [%s] (%v)\n", *g_slaves, err)
	}
	defer f.Close()
	err = json.NewDecoder(f).Decode(&slaves)
	if err != nil {
		log.Panicf("buildbot: could not decode file [%s] (%v)\n", *g_slaves, err)
	}

	//fmt.Printf(">>> %v\n", slaves)

	builders := make([]*Builder, 0, len(slaves))

	for _, slave := range slaves {
		ssh := exec.Command(
			"ssh",
			slave.Addr,
			"echo hello",
		)
		out, err := ssh.CombinedOutput()
		if err != nil {
			log.Printf(
				"slave [%s] did not respond (%v: %s)\n",
				slave.Name, err, string(out),
			)
			continue
		}
		//fmt.Printf("--- slave [%s] ---\n%v\n", slave.Name, string(out))
		err = os.MkdirAll("logs", 0755)
		if err != nil {
			log.Panicf("could create logs directory ! (err=%v)\n", err)
		}

		fname := fmt.Sprintf("logs/%s.txt", slave.Name)
		logfile, err := os.Create(fname)
		if err != nil {
			log.Printf(
				"could not create logfile [%s] for slave [%s] (err=%v)\n",
				fname, slave.Name, err,
			)
		}
		builders = append(builders, &Builder{
			slave: slave,
			w:     logfile,
		})
	}

	fmt.Printf(">>> found the following builders:\n")
	for _, builder := range builders {
		fmt.Printf(" %s \t(%s)\n", builder.slave.Name, builder.slave.Addr)
	}

	fmt.Printf(">>> launching builders... (parallel=%v)\n", *g_parallel)
	done := make(chan BuildReport)
	allgood := true
	for _, builder := range builders {
		fmt.Printf(" %s...\n", builder.slave.Name)
		if *g_parallel {
			go func(builder *Builder) {
				done <- builder.run()
			}(builder)
		} else {
			resp := builder.run()
			if resp.err != nil {
				log.Printf(
					"build failed for slave [%s]:\n%v\nmsg=%s\n",
					resp.slave.Name, resp.err, resp.msg,
				)
				allgood = false
				continue
			}
		}
	}
	fmt.Printf(">>> launching builders... (parallel=%v) [done]\n", *g_parallel)

	if *g_parallel {
		for _ = range builders {
			report := <-done
			if report.err != nil {
				log.Printf(
					"build failed for slave [%s]:\n%v\n",
					report.slave.Name, report.err,
				)
				allgood = false
				continue
			}
		}
	}

	fmt.Printf(">>> all good: %v\n", allgood)
	if !allgood {
		os.Exit(1)
	}
}
