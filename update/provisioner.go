// NB this code was based on https://github.com/hashicorp/packer/blob/81522dced0b25084a824e79efda02483b12dc7cd/provisioner/windows-restart/provisioner.go

package update

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf16"

	"github.com/hashicorp/packer/common"
	"github.com/hashicorp/packer/common/uuid"
	"github.com/hashicorp/packer/helper/config"
	"github.com/hashicorp/packer/packer"
	"github.com/hashicorp/packer/template/interpolate"
)

const (
	elevatedPath                 = "C:/Windows/Temp/packer-windows-update-elevated.ps1"
	elevatedCommand              = "PowerShell -ExecutionPolicy Bypass -OutputFormat Text -File C:/Windows/Temp/packer-windows-update-elevated.ps1"
	windowsUpdatePath            = "C:/Windows/Temp/packer-windows-update.ps1"
	pendingRebootElevatedPath    = "C:/Windows/Temp/packer-windows-update-pending-reboot-elevated.ps1"
	pendingRebootElevatedCommand = "PowerShell -ExecutionPolicy Bypass -OutputFormat Text -File C:/Windows/Temp/packer-windows-update-pending-reboot-elevated.ps1"
	restartCommand               = "shutdown.exe -f -r -t 0 -c \"packer restart\""
	retryableSleep               = 5 * time.Second
	tryCheckReboot               = "shutdown.exe -f -r -t 60"
	abortReboot                  = "shutdown.exe -a"
)

type Config struct {
	common.PackerConfig `mapstructure:",squash"`

	// The timeout for waiting for the machine to restart
	RestartTimeout time.Duration `mapstructure:"restart_timeout"`

	// Instructs the communicator to run the remote script as a
	// Windows scheduled task, effectively elevating the remote
	// user by impersonating a logged-in user.
	Username string `mapstructure:"username"`
	Password string `mapstructure:"password"`

	// The updates search criteria.
	// See the IUpdateSearcher::Search method at https://docs.microsoft.com/en-us/windows/desktop/api/wuapi/nf-wuapi-iupdatesearcher-search.
	SearchCriteria string `mapstructure:"search_criteria"`

	// Filters the installed Windows updates. If no filter is
	// matched the update is NOT installed.
	Filters []string `mapstructure:"filters"`

	// Adds a limit to how many updates are installed at a time
	UpdateLimit int `mapstructure:"update_limit"`

	// Adds a limit to how many retries of an update will be tried before skipping.
	RetryLimit int `mapstructure:"retry_limit"`

	ctx interpolate.Context
}

type Provisioner struct {
	config     Config
	comm       packer.Communicator
	ui         packer.Ui
	cancel     chan struct{}
	cancelLock sync.Mutex
}

func (p *Provisioner) Prepare(raws ...interface{}) error {
	err := config.Decode(&p.config, &config.DecodeOpts{
		Interpolate:        true,
		InterpolateContext: &p.config.ctx,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"execute_command",
			},
		},
	}, raws...)
	if err != nil {
		return err
	}

	if p.config.RestartTimeout == 0 {
		p.config.RestartTimeout = 4 * time.Hour
	}

	if p.config.Username == "" {
		p.config.Username = "SYSTEM"
	}

	var errs error

	if p.config.Username == "" {
		errs = packer.MultiErrorAppend(errs,
			errors.New("Must supply an 'username'"))
	}

	if p.config.UpdateLimit == 0 {
		p.config.UpdateLimit = 1000
	}

	if p.config.RetryLimit == 0 {
		p.config.RetryLimit = 5
	}

	return errs
}

func (p *Provisioner) Provision(ui packer.Ui, comm packer.Communicator) error {
	p.comm = comm
	p.ui = ui

	p.ui.Say("Uploading the Windows update elevated script...")
	var buffer bytes.Buffer
	err := elevatedTemplate.Execute(&buffer, elevatedOptions{
		Username:        p.config.Username,
		Password:        p.config.Password,
		TaskDescription: "Packer Windows update elevated task",
		TaskName:        fmt.Sprintf("packer-windows-update-%s", uuid.TimeOrderedUUID()),
		Command:         p.windowsUpdateCommand(),
	})
	if err != nil {
		fmt.Printf("Error creating elevated template: %s", err)
		return err
	}
	err = p.comm.Upload(
		elevatedPath,
		bytes.NewReader(buffer.Bytes()),
		nil)
	if err != nil {
		return err
	}

	p.ui.Say("Uploading the Windows update check for reboot required elevated script...")
	buffer.Reset()
	err = elevatedTemplate.Execute(&buffer, elevatedOptions{
		Username:        p.config.Username,
		Password:        p.config.Password,
		TaskDescription: "Packer Windows update pending reboot elevated task",
		TaskName:        fmt.Sprintf("packer-windows-update-pending-reboot-%s", uuid.TimeOrderedUUID()),
		Command:         p.windowsUpdateCheckForRebootRequiredCommand(),
	})
	if err != nil {
		fmt.Printf("Error creating elevated template: %s", err)
		return err
	}
	err = p.comm.Upload(
		pendingRebootElevatedPath,
		bytes.NewReader(buffer.Bytes()),
		nil)
	if err != nil {
		return err
	}

	p.ui.Say("Uploading the Windows update script...")
	err = p.comm.Upload(
		windowsUpdatePath,
		bytes.NewReader(MustAsset("windows-update.ps1")),
		nil)
	if err != nil {
		return err
	}

	for {
		restartPending, err := p.update()
		if err != nil {
			return err
		}

		if !restartPending {
			return nil
		}

		err = p.restart()
		if err != nil {
			return err
		}
	}
}

func (p *Provisioner) update() (bool, error) {
	p.ui.Say("Running Windows update...")

	var cmd *packer.RemoteCmd
	err := p.retryable(func() error {
		cmd = &packer.RemoteCmd{Command: elevatedCommand}
		return cmd.StartWithUi(p.comm, p.ui)
	})

	if err != nil {
		return false, err
	}

	switch cmd.ExitStatus {
	case 0:
		return false, nil
	case 101:
		return true, nil
	default:
		return false, fmt.Errorf("Windows update script exited with non-zero exit status: %d", cmd.ExitStatus)
	}
}

func (p *Provisioner) restart() error {
	p.cancelLock.Lock()
	p.cancel = make(chan struct{})
	p.cancelLock.Unlock()

	var cmd *packer.RemoteCmd
	err := p.retryable(func() error {
		cmd = &packer.RemoteCmd{Command: restartCommand}
		return cmd.StartWithUi(p.comm, p.ui)
	})

	if err != nil {
		return err
	}

	if cmd.ExitStatus != 0 {
		return fmt.Errorf("Restart script exited with non-zero exit status: %d", cmd.ExitStatus)
	}

	return waitForRestart(p, p.comm)
}

func waitForRestart(p *Provisioner, comm packer.Communicator) error {
	ui := p.ui
	ui.Say("Waiting for machine to restart...")
	waitDone := make(chan bool, 1)
	timeout := time.After(p.config.RestartTimeout)
	var err error

	p.comm = comm
	var cmd *packer.RemoteCmd
	// Stolen from Vagrant reboot checker
	for {
		log.Printf("Check if machine is rebooting...")
		cmd = &packer.RemoteCmd{Command: tryCheckReboot}
		err = cmd.StartWithUi(comm, ui)
		if err != nil {
			// Couldn't execute, we assume machine is rebooting already
			break
		}
		if cmd.ExitStatus == 1115 || cmd.ExitStatus == 1190 {
			// Reboot already in progress but not completed
			log.Printf("Reboot already in progress, waiting...")
			time.Sleep(10 * time.Second)
		}
		if cmd.ExitStatus == 0 {
			// Cancel reboot we created to test if machine was already rebooting
			cmd = &packer.RemoteCmd{Command: abortReboot}
			cmd.StartWithUi(comm, ui)
			break
		}
	}

	go func() {
		log.Printf("Waiting for machine to become available...")
		err = waitForCommunicator(p)
		waitDone <- true
	}()

	log.Printf("Waiting for machine to reboot with timeout: %s", p.config.RestartTimeout)

WaitLoop:
	for {
		// Wait for either WinRM to become available, a timeout to occur,
		// or an interrupt to come through.
		select {
		case <-waitDone:
			if err != nil {
				ui.Error(fmt.Sprintf("Error waiting for WinRM: %s", err))
				return err
			}
			ui.Say("Machine successfully restarted, moving on")
			close(p.cancel)
			break WaitLoop
		case <-timeout:
			err := fmt.Errorf("Timeout waiting for WinRM")
			ui.Error(err.Error())
			close(p.cancel)
			return err
		case <-p.cancel:
			return fmt.Errorf("Interrupt detected, quitting waiting for machine to restart")
		}
	}

	return nil
}

func waitForCommunicator(p *Provisioner) error {
	for {
		select {
		case <-p.cancel:
			log.Println("Communicator wait canceled, exiting loop")
			return fmt.Errorf("Communicator wait canceled")
		case <-time.After(retryableSleep):
		}

		cmd := &packer.RemoteCmd{Command: pendingRebootElevatedCommand}
		log.Printf("Executing remote command: %s", cmd.Command)

		err := cmd.StartWithUi(p.comm, p.ui)
		if err != nil {
			log.Printf("Communication connection err: %s", err)
			continue
		}
		if cmd.ExitStatus != 0 {
			log.Printf("Machine not yet available (exit status %d)", cmd.ExitStatus)
			continue
		}

		log.Printf("Connected to machine")
		break
	}

	return nil
}

func (p *Provisioner) Cancel() {
	log.Printf("Received interrupt Cancel()")

	p.cancelLock.Lock()
	defer p.cancelLock.Unlock()
	if p.cancel != nil {
		close(p.cancel)
	}
}

// retryable will retry the given function over and over until a
// non-error is returned.
func (p *Provisioner) retryable(f func() error) error {
	startTimeout := time.After(p.config.RestartTimeout)
	for {
		err := f()
		if err == nil {
			return nil
		}

		// Create an error and log it
		err = fmt.Errorf("Retryable error: %s", err)
		log.Print(err.Error())

		// Check if we timed out, otherwise we retry. It is safe to
		// retry since the only error case above is if the command
		// failed to START.
		select {
		case <-startTimeout:
			return err
		default:
			time.Sleep(retryableSleep)
		}
	}
}

func (p *Provisioner) windowsUpdateCommand() string {
	return fmt.Sprintf(
		"PowerShell -ExecutionPolicy Bypass -OutputFormat Text -EncodedCommand %s",
		base64.StdEncoding.EncodeToString(
			encodeUtf16Le(fmt.Sprintf(
				"%s%s%s -UpdateLimit %d -RetryLimit %d",
				windowsUpdatePath,
				searchCriteriaArgument(p.config.SearchCriteria),
				filtersArgument(p.config.Filters),
				p.config.UpdateLimit,
				p.config.RetryLimit))))
}

func (p *Provisioner) windowsUpdateCheckForRebootRequiredCommand() string {
	return fmt.Sprintf(
		"PowerShell -ExecutionPolicy Bypass -OutputFormat Text -EncodedCommand %s",
		base64.StdEncoding.EncodeToString(
			encodeUtf16Le(fmt.Sprintf(
				"%s -OnlyCheckForRebootRequired",
				windowsUpdatePath))))
}

func encodeUtf16Le(s string) []byte {
	d := utf16.Encode([]rune(s))
	b := make([]byte, len(d)*2)
	for i, r := range d {
		b[i*2] = byte(r)
		b[i*2+1] = byte(r >> 8)
	}
	return b
}

func searchCriteriaArgument(searchCriteria string) string {
	if searchCriteria == "" {
		return ""
	}

	var buffer bytes.Buffer

	buffer.WriteString(" -SearchCriteria ")
	buffer.WriteString(escapePowerShellString(searchCriteria))

	return buffer.String()
}

func filtersArgument(filters []string) string {
	if filters == nil {
		return ""
	}

	var buffer bytes.Buffer

	buffer.WriteString(" -Filters ")

	for i, filter := range filters {
		if i > 0 {
			buffer.WriteString(",")
		}
		buffer.WriteString(escapePowerShellString(filter))
	}

	return buffer.String()
}

func escapePowerShellString(value string) string {
	return fmt.Sprintf(
		"'%s'",
		// escape single quotes with another single quote.
		strings.Replace(value, "'", "''", -1))
}
