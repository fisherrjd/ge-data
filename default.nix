{ pkgs ? import
    (fetchTarball {
      name = "jpetrucciani-2026-06-15";
      url = "https://github.com/jpetrucciani/nix/archive/f6fdf3083318280d75a762982ac1ec0c07daeba9.tar.gz";
      sha256 = "0fjrprqgd48mml6ygzyva5sh0nfflm02czw7czm0a8paqw8g2mm8";
    })
    { }
}:
let
  name = "ge-data";
  pg = pkgs.postgresql_16.withPackages (p: with p; [ pgvector timescaledb ]);

  tools = with pkgs; {
    cli = [
      jfmt
      nixup
    ];
    go = [
      go
      go-tools
      gopls
      gcc
    ];
    scripts = pkgs.lib.attrsets.attrValues scripts;
  };

  scripts = with pkgs; {
    pg = __pg {
      postgres = pg;
      # Bind localhost (local tooling: db_reset, ingester) + eldo's tailscale IP
      # so atlantis can reach it over the tailnet. NOT '*': we don't expose the
      # LAN NICs. Tailscale IPs are stable per-node, so hardcoding is safe. Who
      # may connect is still gated by pg_hba.conf (see db_reset) + firewall.
      extra_flags = "-c shared_preload_libraries=timescaledb -c listen_addresses='localhost,100.66.184.28'";
    };
    pg_bootstrap = __pg_bootstrap {
      inherit name;
      postgres = pg;
      # timescaledb is NOT created here: __pg_bootstrap's internal server start
      # has no shared_preload_libraries, so CREATE EXTENSION timescaledb FATALs.
      # It is created by init/01_schema.sql against the preloaded `pg` server.
      extensions = [ "pgcrypto" "uuid-ossp" ];
    };
    pg_shell = __pg_shell { inherit name; postgres = pg; };

    # One-shot local-dev DB prepare: wipe + bootstrap + load init/01_schema.sql
    # (timescaledb extension + the two price hypertables) against a temporary
    # preloaded server, then stop. Run from the repo root, then `__pg` to serve.
    # DESTRUCTIVE: wipes $PGDATA.
    db_reset = writeShellScriptBin "db_reset" ''
      set -euo pipefail
      : "''${PGDATA:?PGDATA not set (enter the nix shell)}"
      : "''${PGPORT:?PGPORT not set (enter the nix shell)}"
      # Network auth secret from .env (gitignored). Needed to set the role password
      # and open pg_hba to the tailscale peer. Set a real value before exposing.
      # Pull just POSTGRES_PASSWORD from .env without executing it (.env holds
      # values with spaces/parens that would break `source`).
      if [ -f .env ]; then
        POSTGRES_PASSWORD="$(grep -E '^POSTGRES_PASSWORD=' .env | tail -1 | cut -d= -f2-)"
      fi
      : "''${POSTGRES_PASSWORD:?set POSTGRES_PASSWORD in .env (network auth)}"
      if [ ! -f init/01_schema.sql ]; then
        echo "run from the repo root (init/01_schema.sql not found)" >&2
        exit 1
      fi
      echo "==> bootstrap (wipes $PGDATA)"
      ${scripts.pg_bootstrap}/bin/__pg_bootstrap -f
      echo "==> start temporary preloaded server"
      ${pg}/bin/postgres -k "$PGDATA" -D "$PGDATA" -p "$PGPORT" \
        -c shared_preload_libraries=timescaledb >/tmp/ge-data-db_reset.log 2>&1 &
      srv=$!
      trap 'kill "$srv" 2>/dev/null || true' EXIT
      echo "==> wait for readiness"
      for _ in $(seq 1 60); do
        ${pg}/bin/pg_isready -h localhost -p "$PGPORT" -q && break
        sleep 0.3
      done
      echo "==> load init/01_schema.sql"
      ${pg}/bin/psql -h localhost -p "$PGPORT" -U ${name} -d ${name} \
        -v ON_ERROR_STOP=1 -f init/01_schema.sql
      echo "==> set role password + open pg_hba to tailscale peer (atlantis)"
      ${pg}/bin/psql -h localhost -p "$PGPORT" -U ${name} -d ${name} -v ON_ERROR_STOP=1 \
        -c "ALTER ROLE \"${name}\" WITH PASSWORD '$POSTGRES_PASSWORD'"
      if ! grep -q "100.88.30.31/32" "$PGDATA/pg_hba.conf"; then
        echo "host all all 100.88.30.31/32 scram-sha-256" >> "$PGDATA/pg_hba.conf"
      fi
      echo "==> stop temporary server"
      kill "$srv"; wait "$srv" 2>/dev/null || true
      trap - EXIT
      echo "OK: local db ready. Run '__pg' to serve on port $PGPORT."
    '';
  };
  paths = pkgs.lib.flatten [ (builtins.attrValues tools) ];
  env = pkgs.buildEnv {
    inherit name paths; buildInputs = paths;
  };
in
(env.overrideAttrs (_: {
  inherit name;
  NIXUP = "0.0.10";
  shellHook = ''
    export PGDATA="$PWD/.db"
    export PGPORT=5433
  '';
})) // { inherit scripts; }
