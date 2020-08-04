package lxc

import "github.com/hashicorp/nomad/plugins/shared/hclspec"

var (
	danglingContainersBlock = hclspec.NewObject(map[string]*hclspec.Spec{
		"enabled": hclspec.NewDefault(
			hclspec.NewAttr("enabled", "bool", false),
			hclspec.NewLiteral(`true`),
		),
		"period": hclspec.NewDefault(
			hclspec.NewAttr("period", "string", false),
			hclspec.NewLiteral(`"5m"`),
		),
		"creation_grace": hclspec.NewDefault(
			hclspec.NewAttr("creation_grace", "string", false),
			hclspec.NewLiteral(`"5m"`),
		),
		"dry_run": hclspec.NewDefault(
			hclspec.NewAttr("dry_run", "bool", false),
			hclspec.NewLiteral(`false`),
		),
	})

	backingstoreBlock = hclspec.NewObject(map[string]*hclspec.Spec{
		"mode": hclspec.NewDefault(
			hclspec.NewAttr("mode", "string", true),
			hclspec.NewAttr("\"dir\""),
		),
		"zfs": hclspec.NewBlock("zfs", false, hclspec.NewObject(map[string]*hclspec.Spec{
			"root": hclspec.NewDefault(
				hclspec.NewAttr("root", "string", true),
				hclspec.NewLiteral("\"tank/lxc\""),
			),
		})),
		"lvm": hclspec.NewBlock("lvm", false, hclspec.NewObject(map[string]*hclspec.Spec{
			"lvname":   hclspec.NewAttr("lvname", "string", false),
			"vgname":   hclspec.NewAttr("vgname", "string", false),
			"thinpool": hclspec.NewAttr("thinpool", "string", false),
		})),
		"rbd": hclspec.NewObject("rbd", false, hclspec.NewObject(map[string]*hclspec.Spec{
			"name": hclspec.NewAttr("name", "string", true),
			"pool": hclspec.NewAttr("pool", "string", true),
		})),
		"fstype": hclspec.NewAttr("fstype", "string", false),
		"fssize": hclspec.NewAttr("fssize", "string", false),
		"dir":    hclspec.NewAttr("dir", "string", false),
	})

	// configSpec is the hcl specification returned by the ConfigSchema RPC
	configSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"enabled": hclspec.NewDefault(
			hclspec.NewAttr("enabled", "bool", false),
			hclspec.NewLiteral("true"),
		),
		"volumes_enabled": hclspec.NewDefault(
			hclspec.NewAttr("volumes_enabled", "bool", false),
			hclspec.NewLiteral("true"),
		),
		"lxc_path": hclspec.NewAttr("lxc_path", "string", false),
		"network_mode": hclspec.NewDefault(
			hclspec.NewAttr("network_mode", "string", false),
			hclspec.NewLiteral("\"bridge\""),
		),
		// garbage collection options
		// default needed for both if the gc {...} block is not set and
		// if the default fields are missing
		"gc": hclspec.NewDefault(hclspec.NewBlock("gc", false, hclspec.NewObject(map[string]*hclspec.Spec{
			"container": hclspec.NewDefault(
				hclspec.NewAttr("container", "bool", false),
				hclspec.NewLiteral("true"),
			),
			"dangling_containers": hclspec.NewDefault(
				hclspec.NewBlock("dangling_containers", false, danglingContainersBlock),
				hclspec.NewLiteral(`{
					enabled = true
					period = "5m"
					creation_grace = "5m"
				}`),
			),
		})), hclspec.NewLiteral(`{
			container = true
			dangling_containers = {
				enabled = true
				period = "5m"
				creation_grace = "5m"
			}
		}`)),
	})

	// taskConfigSpec is the hcl specification for the driver config section of
	// a task within a job. It is returned in the TaskConfigSchema RPC
	taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
		"template":       hclspec.NewAttr("template", "string", true),
		"distro":         hclspec.NewAttr("distro", "string", false),
		"release":        hclspec.NewAttr("release", "string", false),
		"arch":           hclspec.NewAttr("arch", "string", false),
		"image_variant":  hclspec.NewAttr("image_variant", "string", false),
		"image_server":   hclspec.NewAttr("image_server", "string", false),
		"gpg_key_id":     hclspec.NewAttr("gpg_key_id", "string", false),
		"gpg_key_server": hclspec.NewAttr("gpg_key_server", "string", false),
		"disable_gpg":    hclspec.NewAttr("disable_gpg", "string", false),
		"flush_cache":    hclspec.NewAttr("flush_cache", "string", false),
		"force_cache":    hclspec.NewAttr("force_cache", "string", false),
		"template_args":  hclspec.NewAttr("template_args", "list(string)", false),
		"log_level":      hclspec.NewAttr("log_level", "string", false),
		"verbosity":      hclspec.NewAttr("verbosity", "string", false),
		"volumes":        hclspec.NewAttr("volumes", "list(string)", false),
		"network_mode":   hclspec.NewAttr("network_mode", "string", false),
		"port_map":       hclspec.NewAttr("port_map", "list(map(number))", false),
		"parameters":     hclspec.NewAttr("parameters", "list(string)", false),
		"command":        hclspec.NewAttr("command", "string", false),
		"args":           hclspec.NewAttr("args", "list(string)", false),
		"bdev":           backingstoreBlock,
	})
)
