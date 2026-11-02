package track

import (
	"path"
	"strings"
)

// The kinds of address scanners try on every website, as ProbeKind names them.
const (
	// ProbeSource is after source code and version control: /.git/config.
	ProbeSource = "source"
	// ProbeSecrets is after passwords, keys and configuration: /.env.
	ProbeSecrets = "secrets"
	// ProbeWordPress is after WordPress: /wp-login.php.
	ProbeWordPress = "wordpress"
	// ProbeTools is after admin tools and known software: /phpmyadmin.
	ProbeTools = "tools"
	// ProbeScripts is after scripts a Go site has none of: /shell.php.
	ProbeScripts = "scripts"
	// ProbeBackup is after backups and logs: /dump.sql.
	ProbeBackup = "backup"
)

// probeSegments are the parts of an address that give a scanner away.
var probeSegments = map[string]string{
	".git": ProbeSource, ".gitignore": ProbeSource, ".gitconfig": ProbeSource, ".svn": ProbeSource, ".hg": ProbeSource, ".bzr": ProbeSource, ".ds_store": ProbeSource,

	".aws": ProbeSecrets, ".azure": ProbeSecrets, ".gcloud": ProbeSecrets, ".ssh": ProbeSecrets, ".docker": ProbeSecrets, ".kube": ProbeSecrets,
	".npmrc": ProbeSecrets, ".pypirc": ProbeSecrets, ".netrc": ProbeSecrets, ".htpasswd": ProbeSecrets, ".htaccess": ProbeSecrets, ".bash_history": ProbeSecrets,
	"id_rsa": ProbeSecrets, "id_dsa": ProbeSecrets, "id_ed25519": ProbeSecrets, "credentials": ProbeSecrets, "credentials.json": ProbeSecrets,
	"secrets.json": ProbeSecrets, "secrets.yml": ProbeSecrets, "secrets.yaml": ProbeSecrets, "web.config": ProbeSecrets, "appsettings.json": ProbeSecrets,
	"docker-compose.yml": ProbeSecrets, "docker-compose.yaml": ProbeSecrets, "database.yml": ProbeSecrets, "parameters.yml": ProbeSecrets,
	"meta-data": ProbeSecrets, "computemetadata": ProbeSecrets,

	"xmlrpc.php": ProbeWordPress,

	"pma": ProbeTools, "myadmin": ProbeTools, "adminer": ProbeTools, "adminer.php": ProbeTools, "phpinfo": ProbeTools, "phpunit": ProbeTools,
	"_profiler": ProbeTools, "_environment": ProbeTools, "_ignition": ProbeTools, "_debugbar": ProbeTools, "telescope": ProbeTools, "actuator": ProbeTools,
	"server-status": ProbeTools, "server-info": ProbeTools, "cgi-bin": ProbeTools, "jenkins": ProbeTools, "solr": ProbeTools, "boaform": ProbeTools, "hnap1": ProbeTools,
	"owa": ProbeTools, "autodiscover": ProbeTools, "elfinder": ProbeTools, "filemanager": ProbeTools, "fckeditor": ProbeTools, "ckfinder": ProbeTools, "kcfinder": ProbeTools,
	"ckeditor": ProbeTools, "tinymce": ProbeTools, "codemirror": ProbeTools,
}

// probeExtensions are the file extensions that give a scanner away.
var probeExtensions = map[string]string{
	".php": ProbeScripts, ".php3": ProbeScripts, ".php5": ProbeScripts, ".php7": ProbeScripts, ".phtml": ProbeScripts,
	".asp": ProbeScripts, ".aspx": ProbeScripts, ".jsp": ProbeScripts, ".jspx": ProbeScripts, ".cgi": ProbeScripts, ".cfm": ProbeScripts, ".pl": ProbeScripts,

	".sql": ProbeBackup, ".bak": ProbeBackup, ".old": ProbeBackup, ".orig": ProbeBackup, ".swp": ProbeBackup, ".save": ProbeBackup,
	".backup": ProbeBackup, ".dump": ProbeBackup, ".sqlite": ProbeBackup, ".log": ProbeBackup,

	".pem": ProbeSecrets, ".key": ProbeSecrets, ".p12": ProbeSecrets, ".pfx": ProbeSecrets, ".ini": ProbeSecrets, ".conf": ProbeSecrets, ".cfg": ProbeSecrets,
}

// ProbeKind says what a request for the address is after when it is one
// scanners try on every website, and "" when it is not. It goes by the address
// alone, so it cannot tell who asks; a page your site really has at such an
// address still counts as a page.
func ProbeKind(address string) string {
	address = strings.ToLower(address)

	for rest := address; rest != ""; {
		var segment string

		segment, rest, _ = strings.Cut(rest, "/")
		if kind := probeSegment(segment); kind != "" {
			return kind
		}
	}

	return probeExtensions[path.Ext(address)]
}

func probeSegment(segment string) string {
	if kind, ok := probeSegments[segment]; ok {
		return kind
	}

	switch {
	case segment == "":
		return ""
	case strings.HasPrefix(segment, ".env"):
		return ProbeSecrets
	case strings.HasPrefix(segment, "wp-"), strings.Contains(segment, "wordpress"):
		return ProbeWordPress
	case strings.Contains(segment, "phpmyadmin"):
		return ProbeTools
	case strings.HasPrefix(segment, "backup"), strings.HasPrefix(segment, "dump."):
		return ProbeBackup
	}

	return ""
}
