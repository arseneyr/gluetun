package surfshark

import (
	"strconv"

	"github.com/qdm12/gluetun/internal/configuration"
	"github.com/qdm12/gluetun/internal/constants"
	"github.com/qdm12/gluetun/internal/models"
	"github.com/qdm12/gluetun/internal/provider/utils"
)

func (s *Surfshark) BuildConf(connection models.Connection,
	settings configuration.OpenVPN) (lines []string, err error) {
	if settings.Cipher == "" {
		settings.Cipher = constants.AES256gcm
	}

	if settings.Auth == "" {
		settings.Auth = constants.SHA512
	}

	const defaultMSSFix = 1450
	if settings.MSSFix == 0 {
		settings.MSSFix = defaultMSSFix
	}

	lines = []string{
		"client",
		"dev " + settings.Interface,
		"nobind",
		"remote-cert-tls server",
		"ping 15",
		"tls-exit",

		// Surfshark specific
		"tun-mtu 1500",
		"tun-mtu-extra 32",
		"mssfix " + strconv.Itoa(int(settings.MSSFix)),
		"reneg-sec 0",
		"key-direction 1",
		"ping-restart 0",

		// Added constant values
		"auth-nocache",
		"mute-replay-warnings",
		"pull-filter ignore \"auth-token\"", // prevent auth failed loops
		"auth-retry nointeract",
		"suppress-timestamps",

		// Modified variables
		"verb " + strconv.Itoa(settings.Verbosity),
		"auth-user-pass " + constants.OpenVPNAuthConf,
		connection.OpenVPNProtoLine(),
		connection.OpenVPNRemoteLine(),
		"auth " + settings.Auth,
	}

	lines = append(lines, utils.CipherLines(settings.Cipher, settings.Version)...)

	if !settings.Root {
		lines = append(lines, "user "+settings.ProcUser)
		lines = append(lines, "persist-tun")
		lines = append(lines, "persist-key")
	}

	if !settings.IPv6 {
		lines = append(lines, `pull-filter ignore "route-ipv6"`)
		lines = append(lines, `pull-filter ignore "ifconfig-ipv6"`)
	}

	lines = append(lines, utils.WrapOpenvpnCA(
		constants.SurfsharkCertificate)...)
	lines = append(lines, utils.WrapOpenvpnTLSAuth(
		constants.SurfsharkOpenvpnStaticKeyV1)...)

	lines = append(lines, "")

	return lines, nil
}
