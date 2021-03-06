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
	DNSDName             = "dnsd"
	HTTPDName            = "httpd"
	InsecureHTTPDName    = "insecurehttpd"
	MaintenanceName      = "maintenance"
	PlainSocketName      = "plainsocket"
	SerialPortDaemonName = "serialport"
	SimpleIPSvcName      = "simpleipsvcd"
	SMTPDName            = "smtpd"
	SNMPDName            = "snmpd"
	SOCKDName            = "sockd"
	TelegramName         = "telegram"
	AutoUnlockName       = "autounlock"
	PhoneHomeName        = "phonehome"

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
var AllDaemons = []string{
	AutoUnlockName, DNSDName, HTTPDName, InsecureHTTPDName, MaintenanceName, PhoneHomeName,
	PlainSocketName, SerialPortDaemonName, SimpleIPSvcName, SMTPDName, SNMPDName, SOCKDName, TelegramName,
}

/*
ShedOrder is the sequence of daemon names to be taken offline one after another by supervisor, in case of rapid and
repeated program crash. This mechanism is inspired by design of various aircraft abnormal procedure checklists.
The sequence is prioritised this way:
1. System maintenance daemon
2. Non-essential services that do not require authentication/authorisation.
3. Non-essential services that require authentication/authorisation.
4. Heavy services that use significant amount of resources.
5. Essential services.

The supervisor will not shed the last remaining daemon, which is the auto-unlocking daemon, conveniently not present in
this list. The auto-unlocking daemon provides memorised password to unlock data and configuration of other healthy
instances of laitos program on the LAN or Internet.

If program continues to crash rapidly and repeatedly,
*/
var ShedOrder = []string{
	MaintenanceName,                       // 1
	SerialPortDaemonName, SimpleIPSvcName, // 2
	SNMPDName, DNSDName, // 3
	SOCKDName, SMTPDName, HTTPDName, // 4
	InsecureHTTPDName, PlainSocketName, TelegramName, PhoneHomeName, // 5
	// Never shed - AutoUnlockName
}

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
Supervisor manages the lifecycle of laitos main program that runs daemons. In case that main program crashes rapidly,
the supervisor will attempt to isolate the crashing daemon by restarting laitos main program with reduced set of daemons,
helping healthy daemons to stay online as long as possible.
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
	// mainStdout keeps last several KB of program stdout content for failure notification and forwards everything to stdout.
	mainStdout *lalog.ByteLogWriter
	// mainStderr keeps last several KB of program stderr content for failure notification and forward everything to stderr.
	mainStderr *lalog.ByteLogWriter

	logger lalog.Logger
}

// initialise prepares internal states. This function is called at beginning of Start function.
func (sup *Supervisor) initialise() {
	sup.logger = lalog.Logger{
		ComponentName: "supervisor",
		ComponentID:   []lalog.LoggerIDField{{Key: "PID", Value: os.Getpid()}, {Key: "Daemons", Value: sup.DaemonNames}},
	}
	sup.mainStdout = lalog.NewByteLogWriter(os.Stdout, MemoriseOutputCapacity)
	sup.mainStderr = lalog.NewByteLogWriter(os.Stderr, MemoriseOutputCapacity)
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
		time.Duration(misc.GetSystemUptimeSec()*int(time.Second)).String(), time.Since(misc.StartupTime).String(),
		totalMem/1024, usedMem/1024, misc.GetProgramMemoryUsageKB()/1024,
		misc.GetSystemLoad(),
		runtime.NumCPU(), runtime.GOMAXPROCS(0), runtime.NumGoroutine(),
		string(sup.mainStdout.Retrieve(false)),
		string(sup.mainStderr.Retrieve(false)))
	/*
		Instead sending up to inet.MaxMailBodySize bytes (a very generous size) of program output, be on the safe side
		and limit the size to 1MB, better facilitating successful and speedy delivery.
	*/
	if err := sup.MailClient.Send(subject, lalog.LintString(body, 1048576), sup.NotificationRecipients...); err != nil {
		sup.logger.Warning("notifyFailure", "", err, "failed to send failure notification email")
	}
}

// FeedDecryptionPasswordToStdinAndStart starts the main program and writes the universal decryption password into its stdin.
func FeedDecryptionPasswordToStdinAndStart(decryptionPassword string, cmd *exec.Cmd) error {
	// Start laitos main program
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Feed password into its standard input followed by line break
	if _, err := stdin.Write([]byte(decryptionPassword + "\n")); err != nil {
		return err
	}
	return stdin.Close()
}

/*
Start will fork and launch laitos main program and restarts it in case of crash.
If consecutive crashes occur within 20 minutes, each crash will lead to reduced set of daemons being restarted
with the main program. If Email notification recipients are configured, a crash report will be delivered to those
recipients.
The function blocks caller indefinitely.
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
		if err := FeedDecryptionPasswordToStdinAndStart(misc.ProgramDataDecryptionPassword, mainProgram); err != nil {
			sup.logger.Warning("Start", strconv.Itoa(paramChoice), err, "failed to start main program")
			time.Sleep(1 * time.Second)
			sup.notifyFailure(cliFlags, err)
			if time.Now().Unix()-lastAttemptTime < FailureThresholdSec {
				paramChoice++
			}
			time.Sleep(StartAttemptIntervalSec * time.Second)
			continue
		}
		lastAttemptTime = time.Now().Unix()
		if err := mainProgram.Wait(); err != nil {
			sup.logger.Warning("Start", strconv.Itoa(paramChoice), err, "main program has crashed")
			/*
				Unsure what's going on - the main program crashes, the buffer storing latest stderr content just barely
				catches the beginning of a panic message and never the full stack trace. The full panic message
				including stack trace shows up properly in system journal. Let's see if a delay of a second will help.
			*/
			time.Sleep(1 * time.Second)
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
