# if the clush command is in PATH ...requires the clustershell package
command -v clush >/dev/null && {

        ##
        # Access or export the CLUSTER_NODES environment variable
        #
        CLUSTER_NODES() {
                # Export the CLUSTER_NODES variable to the environment
                # ...if input comes from STDIN...
                if [ ! -t 0 ] ; then
                        read stdin
                        # ...export the CLUSTER_NODES variable to the environment
                        export CLUSTER_NODES=$stdin
                # ...if a single command-line arguments is present...
                elif [ $# -eq 1 ] ; then 
                        export CLUSTER_NODES=$@
                # Otherwise if no command-line argument is present...
                elif [ $# -eq 0 ] ; then
                        # ...make sure the CLUSTER_NODES variable is present
                        if [ -n "$CLUSTER_NODES" ]
                        then
                                # append a domain to the node names if missing
                                cluster-node-fqdn "$CLUSTER_NODES"
                        else
                                echo 1>&2 "CLUSTER_NODES environment variable is empty, unset or blank!"
                                echo ""
                        fi
                # Catch all if more then single command-line argument is present
                else
                        echo 1>&2 "Error: No argument or a single nodeset argument required!"
                        echo ""
                fi
        }
       
        ##
        # Forcing a connection timeout on all logins... Fanout should be very
        # conservative to avoid false connection issues when tunneling and 
        # proxy jumping...
        #

        ##
        # Execute command on a nodeset
        #
        cluster-rush() {
                # read the nodeset from the environment variable
                local nodeset=$(CLUSTER_NODES 2>/dev/null)
                # if the nodeset is not empty
                if [ -n "$nodeset" ]
                then
                        clush \
                              --user root \
                              -w "$nodeset" \
                              --connect_timeout=30 \
                              --fanout 6 \
                              $@
                else
                        CLUSTER_NODES
                fi
        }
        alias rush=cluster-rush

        ##
        # Execute command on a nodeset ignoring SSH known host keys and
        #
        cluster-clush-no-checks() {
                # read the nodeset from the environment variable
                local nodeset=$(CLUSTER_NODES 2>/dev/null)
                # if the nodeset is not empty
                if [ -n "$nodeset" ]
                then
                        clush \
                                --user root \
                                -w "$nodeset" \
                                --connect_timeout=30 \
                                --fanout 6 \
                                --options '-o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -q' \
                                $@
                else
                        CLUSTER_NODES
                fi
        }
        alias crush=cluster-clush-no-checks

}
