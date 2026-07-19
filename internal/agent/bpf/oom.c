// +build ignore

#include "vmlinux.h"
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char __license[] SEC("license") = "Dual MIT/GPL";

struct oom_event {
    u32 fpid;         // Trigger process PID
    u32 tpid;         // Victim process PID
    u64 cgroup_id;    // Victim cgroup ID
    u16 oom_score;    // Victim OOM score
    short oom_score_adj; // Victim OOM score adjustment
    char fcomm[16];   // Trigger command name
    char tcomm[16];   // Victim command name
    u64 pages;        // Total pages (approx memory)
    u64 anon_rss;     // Anonymous RSS bytes
    u64 file_rss;     // File RSS bytes
    u64 pgtables;     // Page tables bytes
    char cgroup_name[128]; // Cgroup directory name
    bool is_global_oom; // True if node exhaustion
};

const struct oom_event *unused __attribute__((unused));

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 24); // 16MB ring buffer
} events SEC(".maps");

SEC("kprobe/oom_kill_process")
int BPF_KPROBE(kprobe__oom_kill_process, struct oom_control *oc, const char *message)
{
    struct oom_event *event;
    struct task_struct *victim = NULL;
    struct task_struct *current_task = (struct task_struct *)bpf_get_current_task();
    struct mm_struct *mm = NULL;

    // Read victim task from oom_control
    bpf_core_read(&victim, sizeof(victim), &oc->chosen);
    if (!victim) {
        return 0; // No victim chosen yet
    }

    // Reserve space in ring buffer
    event = bpf_ringbuf_reserve(&events, sizeof(*event), 0);
    if (!event) {
        return 0; // Ringbuf full
    }

    // Populate trigger process details
    event->fpid = bpf_get_current_pid_tgid() >> 32;
    bpf_get_current_comm(&event->fcomm, sizeof(event->fcomm));

    // Populate victim process details
    bpf_core_read(&event->tpid, sizeof(event->tpid), &victim->tgid);
    bpf_core_read_str(&event->tcomm, sizeof(event->tcomm), &victim->comm);

    // Victim OOM details
    struct signal_struct *sig = NULL;
    bpf_core_read(&sig, sizeof(sig), &victim->signal);
    if (sig) {
        bpf_core_read(&event->oom_score_adj, sizeof(event->oom_score_adj), &sig->oom_score_adj);
    }
    
    long chosen_points = 0;
    bpf_core_read(&chosen_points, sizeof(chosen_points), &oc->chosen_points);
    event->oom_score = (u16)chosen_points;

    // Victim total pages from oom_control
    bpf_core_read(&event->pages, sizeof(event->pages), &oc->totalpages);

    // Global vs Container limit
    struct mem_cgroup *memcg = NULL;
    bpf_core_read(&memcg, sizeof(memcg), &oc->memcg);
    event->is_global_oom = (memcg == NULL);

    // Victim Memory details (approximate from mm_struct)
    bpf_core_read(&mm, sizeof(mm), &victim->mm);
    if (mm) {
        // Read page tables bytes
        long pgtables_bytes = 0;
        bpf_core_read(&pgtables_bytes, sizeof(pgtables_bytes), &mm->pgtables_bytes.counter);
        event->pgtables = (u64)pgtables_bytes;

        struct percpu_counter file_pages = {};
        struct percpu_counter anon_pages = {};
        
        if (bpf_core_field_exists(mm->rss_stat[0])) {
            bpf_core_read(&file_pages, sizeof(file_pages), &mm->rss_stat[0]);
            bpf_core_read(&anon_pages, sizeof(anon_pages), &mm->rss_stat[1]);
            
            event->file_rss = (u64)file_pages.count * 4096;
            event->anon_rss = (u64)anon_pages.count * 4096;
        }
    }

    // Victim Cgroup ID
    struct css_set *cgroups = NULL;
    struct cgroup_subsys_state *dfl_cgrp = NULL;
    struct cgroup *cgrp = NULL;
    struct kernfs_node *kn = NULL;
    u64 cgrp_id = 0;

    bpf_core_read(&cgroups, sizeof(cgroups), &victim->cgroups);
    if (cgroups) {
        bpf_core_read(&dfl_cgrp, sizeof(dfl_cgrp), &cgroups->dfl_cgrp);
        if (dfl_cgrp) {
            bpf_core_read(&cgrp, sizeof(cgrp), &dfl_cgrp->cgroup);
            if (cgrp) {
                bpf_core_read(&kn, sizeof(kn), &cgrp->kn);
                if (kn) {
                    bpf_core_read(&cgrp_id, sizeof(cgrp_id), &kn->id);
                    const char *name_ptr = NULL;
                    bpf_core_read(&name_ptr, sizeof(name_ptr), &kn->name);
                    if (name_ptr) {
                        bpf_core_read_str(&event->cgroup_name, sizeof(event->cgroup_name), name_ptr);
                    }
                }
            }
        }
    }
    event->cgroup_id = cgrp_id;

    bpf_ringbuf_submit(event, 0);
    return 0;
}
