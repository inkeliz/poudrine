#!/bin/sh
# From the checkout, run this on the builder and save the report locally:
#   ssh user@builder sh -s < examples/check-poudriere-rebuild.sh > rebuild-report.txt
# To compare the two reported runs, use this command from the checkout:
#   ssh user@builder sh -s -- --compare 2026-11-24_16h38m47s.279112079 2026-11-26_08h56m56s.301942898 < examples/check-poudriere-rebuild.sh > rebuild-comparison.txt
# On the builder, open or follow a log using a full path printed in the report:
#   less /full/path/to/package.log
#   tail -f /full/path/to/package.log
# From your own machine, copy a full log:
#   scp user@builder:/full/path/to/package.log .
# These logs cover individual packages. Early repository cleanup messages
# were printed to the worker's bulk console before package logging started.
# To check a different worker or package on the builder:
#   BASEFS=/your/basefs MASTERNAME=yourjail-yourports sh check-poudriere-rebuild.sh name-version

basefs=${BASEFS:-/usr/local/poudriere}
master=${MASTERNAME:-latest-main}
repo=$basefs/data/packages/$master
runs=$basefs/data/logs/bulk/$master

printf '%s\n' 'Jail versions (.latest is published, .building is staged)'
for file in "$repo/.latest/.jailversion" "$repo/.building/.jailversion"; do
    if [ -f "$file" ] && [ -r "$file" ]; then
        grep -H . "$file"
    else
        printf 'Missing or unreadable: %s\n' "$file"
    fi
done

if [ ! -d "$runs" ] || [ ! -r "$runs" ] || [ ! -x "$runs" ]; then
    printf 'Cannot read build runs under %s\n' "$runs" >&2
    exit 1
fi

# Poudrine names each run with a UTC timestamp. Show the last three logs
# for each package so a later, unrelated run cannot hide the affected one.
show_logs() {
    pkg=$1
    printf '\nLatest retained logs for %s\n' "$pkg"
    set -- "$runs"/[0-9][0-9][0-9][0-9]-*/logs/"$pkg".log
    if [ ! -f "$1" ]; then
        printf 'No matching package log under %s\n' "$runs"
        return
    fi
    printf '%s\n' "$@" | tail -n 3 | while IFS= read -r logfile; do
        printf '%s\n' "$logfile"
        run=${logfile%/logs/*}
        for result in built fetched failed; do
            record=$run/.poudriere.ports.$result
            if [ -f "$record" ] && [ -r "$record" ]; then
                awk -v pkg="$pkg" -v result="$result" '$2 == pkg {print "  " result, $0}' "$record"
            fi
        done
    done
}

package_inputs() {
    run=$1
    pkg=$2
    # Both sources are from this dated run. Stop before the compiler output.
    awk '
        { sub(/^\[[^]]+\][[:space:]]*/, "") }
        NR > 4096 || /^=+<phase:/ { exit }
        /^(Port dir (last git commit|unclean checkout):|Ports top (last git commit|unclean checkout):|Jail OSVERSION:|FLAVOR=)/ { print }
        /^---Begin OPTIONS List---/ { options = 1; next }
        /^---End OPTIONS List---/ { complete = options; exit }
        options && $1 ~ /=(on|off):?$/ { sub(/:$/, "", $1); print "option", $1 }
        END { if (!complete) exit 1 }
    ' "$run/logs/$pkg.log" || return 1
    awk -v pkg="$pkg" '$1 == pkg {print "dependency", $2}' "$run/.poudriere.pkg_deps%"
}

earlier=
if [ "${1:-}" = "--compare" ]; then
    if [ "$#" -lt 3 ] || [ "$2" = "$3" ]; then
        echo 'Usage: --compare EARLIER_RUN LATER_RUN [package-version ...]' >&2
        exit 1
    fi
    earlier=$runs/$2
    later=$runs/$3
    shift 3
fi
if [ "$#" -eq 0 ]; then
    set -- python312-3.12.16_1 gettext-tools-1.0_1
fi
if [ -z "$earlier" ]; then
    for pkg do show_logs "$pkg"; done
    exit 0
fi

printf '\nComparing %s and %s\n' "${earlier##*/}" "${later##*/}"
awk '
    NF < 2 { next }
    FILENAME == ARGV[1] { early[$2] = 1; next }
    { late[$2] = 1 }
    END {
        for (pkg in late) if (pkg in early) repeated++
        printf "Unique package IDs compiled: %d earlier, %d later, %d in both\n",
            length(early), length(late), repeated
    }
' "$earlier/.poudriere.ports.built" "$later/.poudriere.ports.built" || exit 1

for snapshot in "$repo/.latest" "$repo/.building"; do
    [ -d "$snapshot" ] || continue
    grep -H . "$snapshot/.buildname"
    ls -liL "$snapshot/Latest/pkg.pkg" 2>&1
    for pkg do
        ls -liL "$snapshot/All/$pkg.pkg" 2>&1
    done
done

scratch=$(mktemp -d) || exit 1
trap 'rm -rf "$scratch"' EXIT
for pkg do
    printf '\nRecorded port, options and direct dependencies for %s\n' "$pkg"
    if ! package_inputs "$earlier" "$pkg" > "$scratch/earlier" ||
        ! package_inputs "$later" "$pkg" > "$scratch/later"; then
        echo 'Comparison unavailable (incomplete package log or missing dependency graph).'
        continue
    fi
    LC_ALL=C sort -u -o "$scratch/earlier" "$scratch/earlier" || exit 1
    LC_ALL=C sort -u -o "$scratch/later" "$scratch/later" || exit 1
    if diff -u -L earlier -L later "$scratch/earlier" "$scratch/later"; then
        echo 'No differences in these recorded inputs.'
    else
        [ "$?" -eq 1 ] || exit 1
    fi
done
