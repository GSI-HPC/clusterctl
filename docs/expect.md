
# `expect` Scripts

Script | Description
-------|-------------
`expect-ssh` | Execute a command over SSH on target
`expect-scp` | Copy a file over SSH to/from target 
`expect-ssh-sudo` | Execute a command over SSH on target, command requires a subsequent password prompt (for example `sudo`)
`expect-ssh-setpwd` | Execute a command over SSH on target, command requires a subsequent password prompt, and sets and confirms a new password  

The scripts require following environment variables to be set:

Environment Variable | Description
---------------------|----------------
`EXPECT_LOGIN_PASSWORD` | Login password on the target, password used for subsequent prompts (for example `sudo`) as well
`EXPECT_LOGIN_USER` | Login user name on the target
`EXPECT_SET_PASSWORD` | Password to be set & confirmed (for example during user creating)

[^sUPSr]: `expect-*` Scripts, Cluster Tools, GitLab  
<https://git.example.com/hpc/cluster/cluster-tools/-/tree/master/bin>

Plumping…

```bash
# example target BMC...
fqdn=${fqdn:-exe0001.mgmt.hpc.example.org}

# access a BMC in vendor state without tooling 
alias ssh-no-keys='ssh -o PreferredAuthentications=password -o PubkeyAuthentication=no'
ssh-no-keys Administrator@$fqdn
iBMC:/-> exit

# execute a command over SSH...
ssh-no-keys Administrator@$fqdn ipmcget -t user -d list

# with Expect wrapper script
export EXPECT_LOGIN_USER=Administrator
export EXPECT_LOGIN_PASSWORD='<factory default password>'
expect-ssh $fqdn ipmcget -t user -d list
```

A more elaborate example using the `expect` wrapper scripts:

```bash
# use the `admin` account to access the BMC
export EXPECT_LOGIN_USER=admin
# read passwords without echo, keeping them out of the shell history
read -rs EXPECT_LOGIN_PASSWORD && export EXPECT_LOGIN_PASSWORD

# set a password for a new user
read -rs EXPECT_SET_PASSWORD && export EXPECT_SET_PASSWORD
# create a new user on the target BMC...
expect-ssh-setpwd $fqdn ipmcset -d adduser -v $USER
# ...make the user an operator
expect-ssh-sudo $fqdn ipmcset -d privilege -v $USER 3
# ...user activation
expect-ssh-sudo $fqdn ipmcset -t user -d state -v $USER enabled
# ...upload an SSH public key
expect-scp $USER.pub $fqdn:/tmp/$USER.pub
# ...associate the SSH public key to the user
expect-ssh-sudo $fqdn ipmcset -t user -d addpublickey -v $USER /tmp/$USER.pub

# ...clean up
expect-ssh-sudo $fqdn ipmcset -d deluser -v $USER
```


