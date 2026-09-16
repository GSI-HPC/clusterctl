##
# Transform an IP-address to hexadecimal notation
#
cluster-tftp-ip2hex() {
     local ip=$(host $1.${CLUSTER_DOMAIN_SITE:-} | cut -d' ' -f4)
     printf '%02X' $(echo $ip | tr '.' ' ')
     echo
}

##
# Shell into the production TFTP server
#
cluster-tftp-shell() {
        local user=root
        local fqdn=${CLUSTER_HOST_TFTP:?Set CLUSTER_HOST_TFTP environment variable}
        # unless command line arguments are present
        if [[ "$#" -eq 0 ]]
        then
                # forward ssh-agent an login
                ssh -A $user@$fqdn
        else
                # execute a command on remote
                ssh -t $user@$fqdn -C "$@"
        fi
}

##
# Watch the TFTP log-file
#
cluster-tftp-log() {
        cluster-tftp-shell "watch -n 5 'tail -n 20 /var/log/syslog | ip2host'" 
}


##
# Print TFTP responses to cluster nodes from the TFTP server log
#
cluster-tftp-response() {
       # grep for all $CLUSTER_NODE_PREFIX* nodes by default
       local node=${1:-${CLUSTER_NODE_PREFIX:-}}
       cluster-tftp-shell \
               'zgrep atftpd $(find /var/log -name "syslog*" | sort | tac) | ip2host' \
               | grep Serving \
               | grep -e "${CLUSTER_NODE_PREFIX:-}" \
               | cut -d: -f 2- \
               | tr -s ' ' \
               | cut -d' ' -f1-3,6- \
               | grep -e "$node"
}


##
# Manage the PXESrv instance on the TFTP server
#
cluster-tftp-pxesrv() {
        case "${1:-status}" in
                # Check if the PXESrv instance on the TFTP server is running
                status)
                        cluster-tftp-shell 'pgrep -fl pxesrv'
                        ;;
                # Start the PXESrv instance on the TFTP server
                start)
                        cluster-tftp-shell '
                              source /srv/pxesrv/source_me.sh
                              export PXESRV_ROOT=${CLUSTER_TFTP_ROOT:-/srv/tftp}
                              nohup $PXESRV_PATH/pxesrv -p 5678 >/var/log/pxesrv.log 2>&1 &
                              sleep 2
                              disown
                        '
                        ;;
                # Watch the PXESrv instance disabling TFTP boot
                log)
                        cluster-tftp-shell "
                              watch -n 5 'tail -n 20 /var/log/pxesrv.log | grep ^${CLUSTER_NODE_PREFIX:-}'
                        "
                        ;;
                *)
                        echo "cluster-tftp-pxesrv [start|status|log]"
                        ;;
        esac
}

##
# Configure boot into deployment for a given node
#
cluster-tftp-grub-install() {
        # Path to the TFTP server document root directory
        local tftp_path=${CLUSTER_TFTP_GRUB_PATH:-/srv/tftp/grub}
        # Parse command line arguments
        local node=${1:?Specify a node to install}
        local version=${2:-Specify a version to install}
        # Install a cluster execution node by default
        local target=${3:-exec}
        # Grub configuration used to boot the node
        target=$tftp_path/$version/grub.cfg.install-$target
        # Node specific link to the target Grub boot configuration
        node=$tftp_path/$version/grub.cfg-$(cluster-tftp-ip2hex $node)
        # Login to the TFTP server and configure the boot target
        cluster-tftp-shell "
              test -L $node && rm -v $node
              ln -s $target $node
              stat $node | head -1
        "
}

##
# Upload the Grub configuration from this repository to the TFTP server
#
cluster-tftp-grub-config-upload() {
      scp -r \
              ${CLUSTER_KICKSTART_PATH:?Set CLUSTER_KICKSTART_PATH environment variable}/boot/grub \
              root@${CLUSTER_HOST_TFTP:?Set CLUSTER_HOST_TFTP environment variable}:${CLUSTER_TFTP_ROOT:-/srv/tftp}/
}
