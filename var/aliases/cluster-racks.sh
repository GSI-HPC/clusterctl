# Reads the node inventory (hostname;IP;CID;rack;level) from $RACKS_FILE,
# site-config/node-inventory.csv by default. Rack names must match the
# extended regular expression $CLUSTER_RACK_PATTERN.
cluster-racks() {
# following line assumes `source source_me.sh` in the repos' directory
#REPODIR=$( cd -- "$( dirname -- "${BASH_SOURCE[0]}" )" &> /dev/null && pwd )
racksfile="${RACKS_FILE:-$CLUSTER_TOOLS_PATH/site-config/node-inventory.csv}"
rack_pattern="${CLUSTER_RACK_PATTERN:-^[A-Za-z0-9-]+$}"

##
# Parse command-line arguments
#

_VERBOSE=false
# _VERBOSE=${_VERBOSE:-false}
ARGS=$(getopt -o v -l "expanded,folded" -- "$@")
eval set -- "$ARGS"
while true; do
        case "$1" in
        -v|--verbose)
                shift
                export _VERBOSE=true
                ;;
        --)
                shift
                break
                ;;
        *)
                break
                ;;
        esac
done

write_header() {
    printf "%-9s %-12s %-11s %-7s %-2s\n" hostname IP CID rack level
}

print_node_info() {
    if (( $# > 0 ))
    then
        if [[ "$1" =~ ^[A-Za-z0-9-]+$ ]]; then
            if grep -q $1 $racksfile > /dev/null 2>&1; then
                line=$(grep $1 $racksfile)
                IFS=";" read -r hostname ip cid rack level <<< $line
                printf "%9s %-12s %-11s %-7s %-2s\n" $hostname $ip $cid $rack $level
            else
                echo "node \"$1\" not found"
            fi
        else
            echo "argument must be a short node hostname (given was \"$1\")"
        fi
    else
        echo "Error: No node name specified as argument"
    fi
}

print_rack_info() {
    if (( $# > 0 ))
    then
        if [[ "$1" =~ $rack_pattern ]]; then
            if grep -q $1 $racksfile; then
                if [ "$_VERBOSE" = true ] ; then
                    for node in $(grep $1 $racksfile | cut -d";" -f1); do
                        print_node_info $node
                    done
                else
                    echo -n "$1: "; grep $1 $racksfile | cut -d";" -f1 | nodeset -f
                fi
            else
                echo "rack \"$1\" not found"
            fi
        else
            echo "The argument must be a rack name matching \"$rack_pattern\" (given was \"$1\")"
        fi
    else
        echo "Error: No rack name specified as argument"
    fi
}

list_racks() {
    IFS=$'\n' all_racks=($( cut -d";" -f4 $racksfile | sort -u && printf '\0' ))
    for rack in ${all_racks[@]}; do
        #if [ "$_VERBOSE" = true ] ; then
        #    print_rack_info $rack | nodeset -e
        #else
            print_rack_info $rack
        #fi
    done
}

nodes_same_rack() {
    if (( $# > 0 ))
    then
        if [[ "$1" =~ ^[A-Za-z0-9-]+$ ]]; then
            if grep -q $1 $racksfile > /dev/null 2>&1; then
                rack=$(grep $1 $racksfile | cut -d";" -f4)
                print_rack_info $rack
            else
                echo "node \"$1\" not found"
            fi
        else
            echo "argument must be a short node hostname (given was \"$1\")"
        fi
    else
        echo "Error: No node name specified as argument"
    fi
}

local command=help
if (( $# > 0 ))
then
    command=$1
    shift
fi
case "$command" in
    # output all info for the given node
    node-info)
        write_header
        print_node_info $@
    ;;

    # output node list (expanded or folded) for the given rack
    rack-info)
        if [ "$_VERBOSE" = true ] ; then
            write_header
        fi
        print_rack_info $@
    ;;

    # list racks with corresponding (expanded or folded) nodesets
    list-racks)
        if [ "$_VERBOSE" = true ] ; then
            echo "'--verbose' option not valid for 'list-racks' command"
            _VERBOSE="false"
        fi
        list_racks $@
    ;;

    # list nodes in the same rack as the given node
    nodes-same-rack)
        if [ "$_VERBOSE" = true ] ; then
            write_header
        fi
        nodes_same_rack $@
    ;;

*)
    echo "Usage: cluster-racks [-v] node-info|rack-info|list-racks|nodes-same-rack"
    ;;
esac
}
