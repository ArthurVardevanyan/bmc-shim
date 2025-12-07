package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ArthurVardevanyan/bmc-shim/internal/backend"
	"github.com/ArthurVardevanyan/bmc-shim/internal/server"
)

func main() {
	listen := flag.String("listen", ":8080", "address to listen on (e.g. :8080)")
	user := flag.String("user", os.Getenv("BMC_SHIM_USER"), "basic auth username (or BMC_SHIM_USER)")
	pass := flag.String("pass", os.Getenv("BMC_SHIM_PASS"), "basic auth password (or BMC_SHIM_PASS)")
	systemID := flag.String("system-id", "1", "Redfish system ID path segment (single-system mode)")
	beKind := flag.String("backend", "noop", "backend kind: noop|command|homeassistant")
	onCmd := flag.String("on-cmd", "", "command to execute for power ON (backend=command)")
	offCmd := flag.String("off-cmd", "", "command to execute for power OFF (backend=command)")
	haURL := flag.String("ha-url", os.Getenv("BMC_SHIM_HA_URL"), "Home Assistant base URL (backend=homeassistant)")
	haToken := flag.String("ha-token", os.Getenv("BMC_SHIM_HA_TOKEN"), "Home Assistant API token (backend=homeassistant or BMC_SHIM_HA_TOKEN)")
	haEntity := flag.String("ha-entity", os.Getenv("BMC_SHIM_HA_ENTITY"), "Home Assistant entity_id (backend=homeassistant)")
	haSystems := flag.String("systems", os.Getenv("BMC_SHIM_HA_SYSTEMS"), "Comma-separated list of id=entity_id for multi-system (backend=homeassistant)")
	flag.Parse()

	if *user == "" || *pass == "" {
		log.Println("warning: no basic auth configured; use --user/--pass or BMC_SHIM_USER/BMC_SHIM_PASS")
	}

	systems := map[string]backend.Backend{}
	var be backend.Backend
	var err error
	switch *beKind {
	case "noop":
		be = backend.NewNoop()
		systems[*systemID] = be
	case "command":
		be, err = backend.NewCommand(*onCmd, *offCmd)
		if err != nil {
			log.Fatalf("backend init: %v", err)
		}
		systems[*systemID] = be
	case "homeassistant":
		if *haSystems != "" {
			// parse id=entity,id=entity
			entries := strings.Split(*haSystems, ",")
			for _, e := range entries {
				e = strings.TrimSpace(e)
				if e == "" {
					continue
				}
				parts := strings.SplitN(e, "=", 2)
				if len(parts) != 2 {
					log.Fatalf("invalid systems entry: %q (expected id=entity)", e)
				}
				id := strings.TrimSpace(parts[0])
				entity := strings.TrimSpace(parts[1])
				b, berr := backend.NewHomeAssistant(*haURL, *haToken, entity)
				if berr != nil {
					log.Fatalf("backend init (%s): %v", id, berr)
				}
				systems[id] = b
			}
			if len(systems) == 0 {
				log.Fatalf("no valid systems parsed from --systems")
			}
		} else {
			b, berr := backend.NewHomeAssistant(*haURL, *haToken, *haEntity)
			if berr != nil {
				log.Fatalf("backend init: %v", berr)
			}
			systems[*systemID] = b
		}
	default:
		log.Fatalf("unknown backend: %s", *beKind)
	}

	srv := server.New(server.Config{
		Listen:   *listen,
		Username: *user,
		Password: *pass,
		Systems:  systems,
	})

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		if err := srv.Start(); err != nil {
			log.Fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	if err := srv.Shutdown(context.Background()); err != nil {
		log.Printf("shutdown error: %v", err)
	}
}
