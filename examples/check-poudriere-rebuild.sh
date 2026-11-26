#!/bin/sh
# From the checkout, run this on the builder and save the report locally:
#   ssh user@builder sh -s < examples/check-poudriere-rebuild.sh > rebuild-report.txt
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

if [ "$#" -eq 0 ]; then
    set -- python312-3.12.16_1 gettext-tools-1.0_1
fi
for pkg do
    show_logs "$pkg"
done
