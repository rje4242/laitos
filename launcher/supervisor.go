package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/HouzuoGuo/laitos/inet"
	"github.com/HouzuoGuo/laitos/lalog"
	"github.com/HouzuoGuo/laitos/misc"
)

const (
	ConfigFlagName     = "config"     // ConfigFlagName is the CLI string flag that tells a path to configuration file JSON
	SupervisorFlagName = "supervisor" // SupervisorFlagName is the CLI boolean flag that determines whether supervisor should run
	DaemonsFlagName    = "daemons"    // DaemonsFlagName is the CLI string flag of daemon names (comma separated) to launch

	// Individual daemon names as provided by user in CLI to launch laitos:
	DNSDName          = "dnsd"
	HTTPDName         = "httpd"
	InsecureHTTPDName = "insecurehttpd"
	MaintenanceName   = "maintenance"
	PlainSocketName   = "plainsocket"
	SimpleIPSvcName   = "simpleipsvcd"
	SMTPDName         = "smtpd"
	SNMPDName         = "snmpd"
	SOCKDName         = "sockd"
	TelegramName      = "telegram"
	AutoUnlockName    = "autounlock"

	/*
		FailureThresholdSec determines the maximum failure interval for supervisor to tolerate before taking action to shed
		off components.
	*/
	FailureThresholdSec = 20 * 60
	// StartAttemptIntervalSec is the amount of time to wait between supervisor's attempts to start main program.
	StartAttemptIntervalSec = 10
	// MemoriseOutputCapacity is the size of laitos main program output to memorise for notification purpose.
	MemoriseOutputCapacity = 4 * 1024
)

// AllDaemons is an unsorted list of string daemon names.
var AllDaemons = []string{DNSDName, HTTPDName, InsecureHTTPDName, MaintenanceName, SimpleIPSvcName, SNMPDName, PlainSocketName, SMTPDName, SOCKDName, TelegramName, AutoUnlockName}

// ShedOrder is the sequence of daemon names to be taken offline one after another in case of program crash.
var ShedOrder = []string{MaintenanceName, DNSDName, SOCKDName, SNMPDName, SimpleIPSvcName, SMTPDName, HTTPDName, InsecureHTTPDName, PlainSocketName, TelegramName, AutoUnlockName}

/*
RemoveFromFlags removes CLI flag from input flags base on a condition function (true to remove). The input flags must
not contain the leading executable path.
*/
func RemoveFromFlags(condition func(string) bool, flags []string) (ret []string) {
	ret = make([]string, 0, len(flags))
	var connectNext, deleted bool
	for _, str := range flags {
		if strings.HasPrefix(str, "-") {
			connectNext = true
			if condition(str) {
				if strings.Contains(str, "=") {
					connectNext = false
				}
				deleted = true
			} else {
				ret = append(ret, str)
				deleted = false
			}
		} else if !deleted && connectNext || deleted && !connectNext {
			/*
				For keeping this flag, the two conditions are:
				- Previous flag was not deleted, and its value is the current flag: "-flag value"
				- Previous flag was deleted along with its value: "-flag=123 this_value", therefore this value is not
				  related to the deleted flag and shall be kept.
			*/
			ret = append(ret, str)
		}
	}
	return
}

/*
Supervisor manages the lifecycle of laitos main program that runs daemons. In case of main program crash, the supervisor
relaunches the main program using reduced number of daemons, thus ensuring maximum availability of healthy daemons.
*/
type Supervisor struct {
	// CLIFlags are the thorough list of original program flags to launch laitos. This must not include the leading executable path.
	CLIFlags []string
	// NotificationRecipients are the mail address that will receive notification emails generated by this supervisor.
	NotificationRecipients []string
	// MailClient is used for sending notification emails.
	MailClient inet.MailClient
	// DaemonNames are the original set of daemon names that user asked to start.
	DaemonNames []string
	// shedSequence is the sequence at which daemon shedding takes place. Each latter array has one daemon less than the previous.
	shedSequence [][]string
	// mainStdout forwards verbatim main program output to stdout and keeps latest several KB for notification.
	mainStdout *lalog.ByteLogWriter
	// mainStderr forwards verbatim main program output to stdout and keeps latest several KB for notification.
	mainStderr *lalog.ByteLogWriter

	logger lalog.Logger
}

// initialise prepares internal states. This function is called at beginning of Start function.
func (sup *Supervisor) initialise() {
	sup.logger = lalog.Logger{
		ComponentName: "supervisor",
		ComponentID:   []lalog.LoggerIDField{{Key: "PID", Value: os.Getpid()}, {Key: "Daemons", Value: sup.DaemonNames}},
	}
	sup.mainStderr = lalog.NewByteLogWriter(os.Stderr, MemoriseOutputCapacity)
	sup.mainStdout = lalog.NewByteLogWriter(os.Stdout, MemoriseOutputCapacity)
	// Remove daemon names from CLI flags, because they will be appended by GetLaunchParameters.
	sup.CLIFlags = RemoveFromFlags(func(s string) bool {
		return strings.HasPrefix(s, "-"+DaemonsFlagName)
	}, sup.CLIFlags)
	// Construct daemon shedding sequence
	sup.shedSequence = make([][]string, 0, len(sup.DaemonNames))
	remainingDaemons := sup.DaemonNames
	for _, toShed := range ShedOrder {
		// Do not shed the very last daemon
		if len(remainingDaemons) == 1 {
			break
		}
		// Each round has one less daemon in contrast to the previous round
		thisRound := make([]string, 0)
		var willShed bool
		for _, daemon := range remainingDaemons {
			if daemon == toShed {
				willShed = true
			} else {
				thisRound = append(thisRound, daemon)
			}
		}
		if willShed {
			remainingDaemons = thisRound
			sup.shedSequence = append(sup.shedSequence, thisRound)
		}
	}
}

// notifyFailure sends an Email notification to inform administrator about a main program crash or launch failure.
func (sup *Supervisor) notifyFailure(cliFlags []string, launchErr error) {
	if !sup.MailClient.IsConfigured() || sup.NotificationRecipients == nil || len(sup.NotificationRecipients) == 0 {
		sup.logger.Warning("notifyFailure", "", nil, "will not send Email notification due to missing recipients or mail client config")
		return
	}

	publicIP := inet.GetPublicIP()
	usedMem, totalMem := misc.GetSystemMemoryUsageKB()

	subject := inet.OutgoingMailSubjectKeyword + "-supervisor has detected a failure on " + publicIP
	body := fmt.Sprintf(`
Failure: %v
CLI flags: %v

Clock: %s
Sys/prog uptime: %s / %s
Total/used/prog mem: %d / %d / %d MB
Sys load: %s
Num CPU/GOMAXPROCS/goroutines: %d / %d / %d

Latest stdout: %s

Latest stderr: %s
`, launchErr,
		cliFlags,
		time.Now().String(),
		time.Duration(misc.GetSystemUptimeSec()*int(time.Second)).String(), time.Now().Sub(misc.StartupTime).String(),
		totalMem/1024, usedMem/1024, misc.GetProgramMemoryUsageKB()/1024,
		misc.GetSystemLoad(),
		runtime.NumCPU(), runtime.GOMAXPROCS(0), runtime.NumGoroutine(),
		string(sup.mainStdout.Retrieve(false)),
		string(sup.mainStderr.Retrieve(false)))

	if err := sup.MailClient.Send(subject, inet.LintMailBody(body), sup.NotificationRecipients...); err != nil {
		sup.logger.Warning("notifyFailure", "", err, "failed to send failure notification email")
	}
}

// FeedDecryptionPasswordToStdinAndStart starts the main program and writes the universal decryption password into its stdin.
func FeedDecryptionPasswordToStdinAndStart(decryptionPassword []byte, cmd *exec.Cmd) error {
	// Start laitos main program
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Feed password into its standard input followed by line break
	if _, err := stdin.Write([]byte(string(decryptionPassword) + "\n")); err != nil {
		return err
	}
	return stdin.Close()
}

/*
Start will fork and launch laitos main program. If the main program crashes repeatedly within 20 minutes, the supervisor
will restart the main program with a reduced set of features and send a notification email.
The function blocks caller and runs forever.
*/
func (sup *Supervisor) Start() {
	sup.initialise()
	paramChoice := 0
	lastAttemptTime := time.Now().Unix()
	executablePath, err := os.Executable()
	if err != nil {
		sup.logger.Abort("Start", "", err, "failed to determine path to this program executable")
		return
	}

	for {
		cliFlags, _ := sup.GetLaunchParameters(paramChoice)
		sup.logger.Info("Start", strconv.Itoa(paramChoice), nil, "attempting to start main program with CLI flags - %v", cliFlags)

		mainProgram := exec.Command(executablePath, cliFlags...)
		mainProgram.Stdout = sup.mainStdout
		mainProgram.Stderr = sup.mainStderr
		if err := FeedDecryptionPasswordToStdinAndStart(misc.UniversalDecryptionKey, mainProgram); err != nil {
			sup.logger.Warning("Start", strconv.Itoa(paramChoice), err, "failed to start main program")
			sup.notifyFailure(cliFlags, err)
			if time.Now().Unix()-lastAttemptTime < FailureThresholdSec {
				paramChoice++
			}
			time.Sleep(StartAttemptIntervalSec * time.Second)
			continue
		}
		if err := mainProgram.Wait(); err != nil {
			sup.logger.Warning("Start", strconv.Itoa(paramChoice), err, "main program has crashed")
			sup.notifyFailure(cliFlags, err)
			if time.Now().Unix()-lastAttemptTime < FailureThresholdSec {
				paramChoice++
			}
			time.Sleep(StartAttemptIntervalSec * time.Second)
			continue
		}
		// laitos main program is not supposed to exit, therefore, restart it in the next iteration even if it exits normally.
	}
}

/*
GetLaunchParameters returns the parameters used for launching laitos program for the N-th attempt.
The very first attempt is the 0th attempt.
*/
func (sup *Supervisor) GetLaunchParameters(nthAttempt int) (cliFlags []string, daemonNames []string) {
	addFlags := make([]string, 0, 10)
	cliFlags = make([]string, len(sup.CLIFlags))
	copy(cliFlags, sup.CLIFlags)
	daemonNames = make([]string, len(sup.DaemonNames))
	copy(daemonNames, sup.DaemonNames)

	if nthAttempt >= 0 {
		// The first attempt is a normal start, it tells laitos not to run supervisor.
		cliFlags = RemoveFromFlags(func(f string) bool {
			return strings.HasPrefix(f, "-"+SupervisorFlagName)
		}, cliFlags)
		addFlags = append(addFlags, "-"+SupervisorFlagName+"=false")
	}
	if nthAttempt >= 1 {
		/*
			The second attempt removes all but essential program flag (-config), this means system environment
			will not be altered by the advanced start option such as -gomaxprocs.
		*/
		cliFlags = RemoveFromFlags(func(f string) bool {
			return !strings.HasPrefix(f, "-"+ConfigFlagName)
		}, cliFlags)
	}
	if nthAttempt > 1 && nthAttempt < len(sup.DaemonNames)+1 {
		// More attempts will shed daemons
		daemonNames = sup.shedSequence[nthAttempt-2]
	}
	if nthAttempt > len(sup.DaemonNames)+1 {
		// After shedding daemons, further attempts will not shed any daemons but only remove non-essential flags.
		copy(cliFlags, sup.CLIFlags)
		copy(daemonNames, sup.DaemonNames)
	}
	// Put new flags and new set of daemons into CLI flags
	cliFlags = append(cliFlags, addFlags...)
	cliFlags = append(cliFlags, "-"+DaemonsFlagName, strings.Join(daemonNames, ","))
	return
}
