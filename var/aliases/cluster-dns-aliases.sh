##
# Resolve DNS aliases for all submit nodes
#
cluster-dns-aliases() {
        cluster-login -u $USER -n ${CLUSTER_HOST_POOL:-} $@ -- '
		for name in ${CLUSTER_SLURM_PARTITIONS:-} ; do
                        host $name.${CLUSTER_DOMAIN_HPC:-} | ip2host | cut -d" " -f1,4 ;
                done
        ' | column -t
}

