sub vmstatus {
    my ($opt_vmid, $full) = @_;

    my $res = {};

    my $storecfg = PVE::Storage::config();

    my $list = vzlist();
    my $defaults = load_defaults();

    my ($uptime) = PVE::ProcFSTools::read_proc_uptime(1);

    my $cpucount = $cpuinfo->{cpus} || 1;

    foreach my $vmid (keys %$list) {
        next if $opt_vmid && ($vmid ne $opt_vmid);

        my $conf = PVE::QemuConfig->load_config($vmid);

        my $d = { vmid => int($vmid) };
        $d->{pid} = int($list->{$vmid}->{pid}) if $list->{$vmid}->{pid};

        # fixme: better status?
        $d->{status} = $list->{$vmid}->{pid} ? 'running' : 'stopped';

        my $size = PVE::QemuServer::Drive::bootdisk_size($storecfg, $conf);
        if (defined($size)) {
            $d->{disk} = 0; # no info available
            $d->{maxdisk} = $size;
        } else {
            $d->{disk} = 0;
            $d->{maxdisk} = 0;
        }

        $d->{cpus} =
            ($conf->{sockets} || $defaults->{sockets}) * ($conf->{cores} || $defaults->{cores});
        $d->{cpus} = $cpucount if $d->{cpus} > $cpucount;
        $d->{cpus} = $conf->{vcpus} if $conf->{vcpus};

        $d->{name} = $conf->{name} || "VM $vmid";
        $d->{maxmem} = get_current_memory($conf->{memory}) * (1024 * 1024);

        if ($conf->{balloon}) {
            $d->{balloon_min} = $conf->{balloon} * (1024 * 1024);
            $d->{shares} =
                defined($conf->{shares})
                ? $conf->{shares}
                : $defaults->{shares};
        }

        $d->{uptime} = 0;
        $d->{cpu} = 0;
        $d->{mem} = 0;
        $d->{memhost} = 0;

        $d->{netout} = 0;
        $d->{netin} = 0;

        $d->{template} = 1 if PVE::QemuConfig->is_template($conf);

        $d->{serial} = 1 if conf_has_serial($conf);
        $d->{lock} = $conf->{lock} if $conf->{lock};
        $d->{tags} = $conf->{tags} if defined($conf->{tags});

        $res->{$vmid} = $d;
    }

    my $netdev = PVE::ProcFSTools::read_proc_net_dev();
    foreach my $dev (keys %$netdev) {
        next if $dev !~ m/^tap([1-9]\d*)i/;
        my $vmid = $1;
        my $d = $res->{$vmid};
        next if !$d;

        $d->{netout} += $netdev->{$dev}->{receive};
        $d->{netin} += $netdev->{$dev}->{transmit};

        if ($full) {
            $d->{nics}->{$dev}->{netout} = int($netdev->{$dev}->{receive});
            $d->{nics}->{$dev}->{netin} = int($netdev->{$dev}->{transmit});
        }

    }

    my $ctime = gettimeofday;

    foreach my $vmid (keys %$list) {

        my $d = $res->{$vmid};
        my $pid = $d->{pid};
        next if !$pid;

        my $pstat = PVE::ProcFSTools::read_proc_pid_stat($pid);
        next if !$pstat; # not running

        my $used = $pstat->{utime} + $pstat->{stime};

        $d->{uptime} = int(($uptime - $pstat->{starttime}) / $cpuinfo->{user_hz});

        my $cgroup = PVE::QemuServer::CGroup->new($vmid);
        my $cgroup_mem = eval { $cgroup->get_memory_stat() } // {};
        warn "unable to get memory stat for $vmid - $@" if $@;
        $d->{memhost} = $cgroup_mem->{mem} // 0;

        $d->{mem} = $d->{memhost}; # default to cgroup, balloon info can override this below

        my $pressures = PVE::ProcFSTools::read_cgroup_pressure("qemu.slice/${vmid}.scope");
        $d->{pressurecpusome} = $pressures->{cpu}->{some}->{avg10} * 1;
        $d->{pressurecpufull} = $pressures->{cpu}->{full}->{avg10} * 1;
        $d->{pressureiosome} = $pressures->{io}->{some}->{avg10} * 1;
        $d->{pressureiofull} = $pressures->{io}->{full}->{avg10} * 1;
        $d->{pressurememorysome} = $pressures->{memory}->{some}->{avg10} * 1;
        $d->{pressurememoryfull} = $pressures->{memory}->{full}->{avg10} * 1;

        my $old = $last_proc_pid_stat->{$pid};
        if (!$old) {
            $last_proc_pid_stat->{$pid} = {
                time => $ctime,
                used => $used,
                cpu => 0,
            };
            next;
        }

        my $dtime = ($ctime - $old->{time}) * $cpucount * $cpuinfo->{user_hz};

        if ($dtime > 1000) {
            my $dutime = $used - $old->{used};

            $d->{cpu} = (($dutime / $dtime) * $cpucount) / $d->{cpus};
            $last_proc_pid_stat->{$pid} = {
                time => $ctime,
                used => $used,
                cpu => $d->{cpu},
            };
        } else {
            $d->{cpu} = $old->{cpu};
        }
    }

    return $res if !$full;

    my $qmpclient = PVE::QMPClient->new();

    my $ballooncb = sub {
        my ($vmid, $resp) = @_;

        my $info = $resp->{'return'};
        return if !$info->{max_mem};

        my $d = $res->{$vmid};

        # use memory assigned to VM
        $d->{maxmem} = $info->{max_mem};
        $d->{balloon} = $info->{actual};

        if (defined($info->{total_mem}) && defined($info->{free_mem})) {
            $d->{mem} = $info->{total_mem} - $info->{free_mem};
            $d->{freemem} = $info->{free_mem};
        }

        $d->{ballooninfo} = $info;
    };

    my $blockstatscb = sub {
        my ($vmid, $resp) = @_;
        my $data = $resp->{'return'} || [];
        my $totalrdbytes = 0;
        my $totalwrbytes = 0;

        for my $blockstat (@$data) {
            $totalrdbytes = $totalrdbytes + $blockstat->{stats}->{rd_bytes};
            $totalwrbytes = $totalwrbytes + $blockstat->{stats}->{wr_bytes};

            # With the switch to -blockdev, the block backend in QEMU has no name, so need to also
            # consider the qdev ID. Note that empty CD-ROM drives do not have a node name, so that
            # cannot be used as a fallback instead of the qdev ID.
            my $drive_id;
            if ($blockstat->{device}) {
                $drive_id = $blockstat->{device} =~ s/drive-//r;
            } elsif ($blockstat->{qdev}) {
                $drive_id = PVE::QemuServer::Blockdev::qdev_id_to_drive_id($blockstat->{qdev});
            } else {
                print "blockstats callback: unexpected missing drive ID\n";
                next;
            }

            $res->{$vmid}->{blockstat}->{$drive_id} = $blockstat->{stats};
        }
        $res->{$vmid}->{diskread} = $totalrdbytes;
        $res->{$vmid}->{diskwrite} = $totalwrbytes;
    };

    my $machinecb = sub {
        my ($vmid, $resp) = @_;
        my $data = $resp->{'return'} || [];

        $res->{$vmid}->{'running-machine'} =
            PVE::QemuServer::Machine::current_from_query_machines($data);
    };

    my $versioncb = sub {
        my ($vmid, $resp) = @_;
        my $data = $resp->{'return'} // {};
        my $version = 'unknown';

        if (my $v = $data->{qemu}) {
            $version = $v->{major} . "." . $v->{minor} . "." . $v->{micro};
        }

        $res->{$vmid}->{'running-qemu'} = $version;
    };

    my $proxmox_support_cb = sub {
        my ($vmid, $resp) = @_;
        $res->{$vmid}->{'proxmox-support'} = $resp->{'return'} // {};
    };

    my $statuscb = sub {
        my ($vmid, $resp) = @_;

        my $qmp_peer = vm_qmp_peer($vmid);

        $qmpclient->queue_cmd($qmp_peer, $proxmox_support_cb, 'query-proxmox-support');
        $qmpclient->queue_cmd($qmp_peer, $blockstatscb, 'query-blockstats');
        $qmpclient->queue_cmd($qmp_peer, $machinecb, 'query-machines');
        $qmpclient->queue_cmd($qmp_peer, $versioncb, 'query-version');
        # this fails if balloon driver is not loaded, so this must be
        # the last command (following command are aborted if this fails).
        $qmpclient->queue_cmd($qmp_peer, $ballooncb, 'query-balloon');

        my $status = 'unknown';
        if (!defined($status = $resp->{'return'}->{status})) {
            warn "unable to get VM status\n";
            return;
        }

        $res->{$vmid}->{qmpstatus} = $resp->{'return'}->{status};
    };

    foreach my $vmid (keys %$list) {
        next if $opt_vmid && ($vmid ne $opt_vmid);
        next if !$res->{$vmid}->{pid}; # not running
        $qmpclient->queue_cmd(vm_qmp_peer($vmid), $statuscb, 'query-status');
    }

    $qmpclient->queue_execute(undef, 2);

    foreach my $vmid (keys %$list) {
        next if $opt_vmid && ($vmid ne $opt_vmid);
        $res->{$vmid}->{qmpstatus} = $res->{$vmid}->{status} if !$res->{$vmid}->{qmpstatus};
    }

    return $res;
}
