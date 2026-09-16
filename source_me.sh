# Find the correct path even if dereferenced by a link
__source=$0

if [[ "$__source" == *bash* ]]; then
  __source=${BASH_SOURCE[0]}
fi

__dir="$( dirname $__source )"
while [ -h $__source ]
do
  __source="$( readlink "$__source" )"
  [[ $__source != /* ]] && __source="$__dir/$__source"
  __dir="$( cd -P "$( dirname "$__source" )" && pwd )"
done
__dir="$( cd -P "$( dirname "$__source" )" && pwd )"

export CLUSTER_TOOLS_PATH=$__dir
unset __dir
unset __source

# Source cluster-specific configuration (domains, hostnames, networks) before
# the aliases, which derive further variables from it
__site_config_dir="$CLUSTER_TOOLS_PATH/site-config"
for __cfg in \
    "$__site_config_dir/domains.conf" \
    "$__site_config_dir/hostnames.conf" \
    "$__site_config_dir/networks.conf"; do
  [ -f "$__cfg" ] && source "$__cfg"
done
unset __cfg __site_config_dir

for file in `\ls $CLUSTER_TOOLS_PATH/var/aliases/*.sh`
do
        source $file
done

export PATH=$CLUSTER_TOOLS_PATH/bin:$PATH
PATH=$(echo "$PATH" | awk -v RS=':' -v ORS=":" '!a[$1]++{if (NR > 1) printf ORS; printf $a[$1]}')

export CLUSTER_LOGIN_SSH_KNOWN_HOSTS=${CLUSTER_LOGIN_SSH_KNOWN_HOSTS:-$CLUSTER_TOOLS_PATH/site-config/ssh-known-hosts}

# ClusterShell configuration files for all clusters
export CLUSTERSHELL_CFGDIR=$CLUSTER_TOOLS_PATH/var/clustershell
