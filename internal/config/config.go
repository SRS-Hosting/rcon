// Package config is the rcon command's configuration.
package config

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// EnvPrefix is prepended to every environment variable, so the command reads
// RCON_PASSWORD and RCON_HOST: the same names the sibling services already take,
// and the same ones the game server's own pod already exports.
const EnvPrefix = "RCON_"

// MaxTimeoutSeconds bounds timeoutSeconds. A command-line tool that appears to
// hang is worse than one that gives up and says so, and callers such as a
// container lifecycle hook have their own deadline well under an hour.
const MaxTimeoutSeconds = 3600

const maxPort = 65535

// Config is the command's configuration.
//
// Address and Host/Port both exist because two conventions are already in use:
// the deployed command line passes a single host:port, while the sibling
// services are configured with RCON_HOST and RCON_PORT separately. Supporting
// both means one environment configures all of them; Address wins when set.
type Config struct {
	Address string `name:"address" description:"address of the RCON server as host:port; overrides host and port"`
	Host    string `name:"host" default:"127.0.0.1" description:"hostname or IP of the RCON server"`
	// Ports and timeouts are plain ints rather than sized types: configulator
	// assigns YAML numbers through reflection without a range check, so a
	// narrower field would silently wrap 70000 to 4464 and -1 to 65535 where an
	// int lets Validate reject both.
	Port     int    `name:"port" default:"27015" description:"TCP port of the RCON server"`
	Password string `name:"password" description:"RCON password; prefer the environment variable over an argument"`
	// Expressed in seconds rather than as a time.Duration because configulator
	// parses integer fields with strconv, so a "10s" default would not load.
	TimeoutSeconds int `name:"timeoutSeconds" default:"10" description:"deadline in seconds covering a whole RCON exchange: connect, authenticate, command, response"`
}

// Addr returns the address to connect to.
func (c Config) Addr() string {
	if c.Address != "" {
		return c.Address
	}
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// Timeout returns the deadline for one complete exchange.
func (c Config) Timeout() time.Duration {
	return time.Duration(c.TimeoutSeconds) * time.Second
}

// Validate reports every problem with the configuration at once, so a bad
// invocation does not have to be fixed one run at a time.
func (c Config) Validate() error {
	var errs []error

	if c.Address != "" {
		// Checked here rather than left to the dialler so a missing port is
		// reported as the typo it is, instead of as a connection failure.
		errs = append(errs, validateAddress(c.Address)...)
	} else {
		if c.Host == "" {
			errs = append(errs, errors.New("host must not be empty"))
		}
		if c.Port < 1 || c.Port > maxPort {
			errs = append(errs, fmt.Errorf("port %d must be between 1 and %d", c.Port, maxPort))
		}
	}
	if c.TimeoutSeconds < 1 || c.TimeoutSeconds > MaxTimeoutSeconds {
		errs = append(errs, fmt.Errorf("timeoutSeconds %d must be between 1 and %d",
			c.TimeoutSeconds, MaxTimeoutSeconds))
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid configuration: %w", errors.Join(errs...))
	}
	return nil
}

func validateAddress(address string) []error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return []error{fmt.Errorf("address %q must be host:port: %w", address, err)}
	}

	var errs []error
	if host == "" {
		errs = append(errs, fmt.Errorf("address %q has no host", address))
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		errs = append(errs, fmt.Errorf("address %q has a non-numeric port %q", address, port))
	} else if number < 1 || number > maxPort {
		errs = append(errs, fmt.Errorf("address %q port %d must be between 1 and %d", address, number, maxPort))
	}
	return errs
}
