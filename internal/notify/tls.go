package notify

import "crypto/tls"

// tlsConfig is the TLS settings every outbound notification uses.
//
// Its own file so that "is there an InsecureSkipVerify anywhere" is answerable
// by opening one twelve-line file, and so that adding one is a diff nobody can
// miss.
//
// There is no configuration that relaxes this. A webhook URL is a bearer
// credential and a notification names containers and failures; a destination
// whose certificate cannot be verified is a destination HarborMaster does not
// talk to. An operator with an internal receiver behind a private CA installs
// that CA in the image's trust store, which is a deployment concern rather than
// an application setting.
func tlsConfig() *tls.Config {
	return &tls.Config{
		// TLS 1.2 floor. 1.0 and 1.1 are withdrawn, and the destinations this
		// package talks to -- Slack, Discord, Teams, and any modern receiver --
		// have supported 1.2 for a decade.
		MinVersion: tls.VersionTLS12,
		// Certificate verification is ON, and there is deliberately no field
		// here that turns it off.
	}
}
