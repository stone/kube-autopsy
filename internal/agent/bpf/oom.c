// +build ignore

#include "vmlinux.h"

#if defined(__TARGET_ARCH_arm64)
// vmlinux.h is dumped from an x86_64 kernel, where "struct user_pt_regs" is
// only forward-declared. arm64's PT_REGS_PARM* macros dereference it, so the
// type has to be complete here. This is the arm64 UAPI definition from
// uapi/asm/ptrace.h, which is frozen ABI and cannot change.
struct user_pt_regs {
    __u64 regs[31];
    __u64 sp;
    __u64 pc;
    __u64 pstate;
};
#endif

#include <bpf/bpf_helpers.h>
#include <bpf/bpf_core_read.h>
#include <bpf/bpf_tracing.h>

char __license[] SEC("license") = "Dual MIT/GPL";

struct oom_event {
    u32 fpid;         // Trigger process PID
    u32 tpid;         // Victim process PID
    u64 cgroup_id;    // Victim cgroup ID
    // Kernel "badness" points for the victim, in pages. This is oom_control's
    // chosen_points, not the 0..1000 value exposed as /proc/<pid>/oom_score.
    s64 oom_score;
    s16 oom_score_adj;   // Victim OOM score adjustment
    char fcomm[16];      // Trigger command name
    char tcomm[16];      // Victim command name
    // Total pages available to the OOM scope: the memcg limit for a container
    // OOM, or the node's total RAM for a global OOM. This is a capacity, NOT
    // the victim's consumption.
    u64 scope_total_pages;
    u64 anon_rss_pages;  // Victim anonymous RSS, in pages
    u64 file_rss_pages;  // Victim file-backed RSS, in pages
    // Victim shared-memory RSS, in pages. The kernel's get_mm_rss() is
    // file + anon + shmem, and oom_badness() scores the victim on that total, so
    // omitting this made victim_rss disagree with the oom_score reported beside
    // it for any workload with mapped shared memory (Postgres shared buffers,
    // SysV/POSIX shm, mmap'd /dev/shm).
    u64 shmem_rss_pages;
    // Victim swap entries, in pages. Also part of oom_badness()'s total.
    u64 swap_pages;
    u64 pgtables_bytes;  // Victim page tables, already in bytes
    char cgroup_name[128]; // Cgroup directory name
    bool is_global_oom;  // True if node exhaustion
    // False when the running kernel's mm_struct layout was not recognised, in
    // which case the RSS page counts above are meaningless and must be ignored.
    bool rss_valid;
};

const struct oom_event *unused __attribute__((unused));

// Linux 6.2 (commit f1a7941243c1 "mm: convert mm's rss stats into
// percpu_counter") replaced
//
//     struct mm_rss_stat { atomic_long_t count[NR_MM_COUNTERS]; } rss_stat;
//
// with
//
//     struct percpu_counter rss_stat[NR_MM_COUNTERS];
//
// vmlinux.h describes the newer layout, so the older one is declared here as a
// CO-RE flavor: the "___pre62" suffix is stripped during relocation, so these
// match the kernel's "mm_rss_stat" and "mm_struct" when the target actually has
// them. Without this, kernels older than 6.2 (RHEL 9, Ubuntu 22.04, Amazon
// Linux 2023) silently produce no RSS figures at all.
struct mm_rss_stat___pre62 {
    atomic_long_t count[NR_MM_COUNTERS];
};

struct mm_struct___pre62 {
    struct mm_rss_stat___pre62 rss_stat;
};

// clamp_counter turns a kernel page counter into a non-negative value. Both the
// pre-6.2 atomic counters and the percpu_counter batched totals can read
// transiently negative while a task is tearing down, and the kernel clamps them
// the same way in get_mm_counter(). Without this a negative count would be cast
// to a huge u64 and published as a nonsensical byte figure.
static __always_inline u64 clamp_counter(long value)
{
    return value < 0 ? 0 : (u64)value;
}

struct {
    __uint(type, BPF_MAP_TYPE_RINGBUF);
    __uint(max_entries, 1 << 24); // 16MB ring buffer
} events SEC(".maps");

SEC("kprobe/oom_kill_process")
int BPF_KPROBE(kprobe__oom_kill_process, struct oom_control *oc, const char *message)
{
    struct oom_event *event;
    struct task_struct *victim = NULL;
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

    // bpf_ringbuf_reserve does not zero the reservation, and several fields
    // below are only written conditionally. Without this, a field that is not
    // populated would carry stale bytes from a previous event.
    __builtin_memset(event, 0, sizeof(*event));

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
        s16 score_adj = 0;
        bpf_core_read(&score_adj, sizeof(score_adj), &sig->oom_score_adj);
        event->oom_score_adj = score_adj;
    }

    // chosen_points is a full-width long: truncating it loses the score
    // entirely for any victim larger than about 256MB.
    s64 chosen_points = 0;
    bpf_core_read(&chosen_points, sizeof(chosen_points), &oc->chosen_points);
    event->oom_score = chosen_points;

    // Capacity of the OOM scope, in pages.
    bpf_core_read(&event->scope_total_pages, sizeof(event->scope_total_pages), &oc->totalpages);

    // Global vs Container limit
    struct mem_cgroup *memcg = NULL;
    bpf_core_read(&memcg, sizeof(memcg), &oc->memcg);
    event->is_global_oom = (memcg == NULL);

    // Victim memory details from mm_struct.
    bpf_core_read(&mm, sizeof(mm), &victim->mm);
    if (mm) {
        // pgtables_bytes is a byte count, not a page count.
        long pgtables_bytes = 0;
        bpf_core_read(&pgtables_bytes, sizeof(pgtables_bytes), &mm->pgtables_bytes.counter);
        event->pgtables_bytes = (u64)pgtables_bytes;

        if (bpf_core_type_exists(struct mm_rss_stat___pre62)) {
            // Kernel < 6.2: an array of atomic counters, in pages.
            struct mm_struct___pre62 *mm_old = (struct mm_struct___pre62 *)mm;
            long count = 0;

            bpf_core_read(&count, sizeof(count), &mm_old->rss_stat.count[MM_FILEPAGES]);
            event->file_rss_pages = clamp_counter(count);

            bpf_core_read(&count, sizeof(count), &mm_old->rss_stat.count[MM_ANONPAGES]);
            event->anon_rss_pages = clamp_counter(count);

            bpf_core_read(&count, sizeof(count), &mm_old->rss_stat.count[MM_SHMEMPAGES]);
            event->shmem_rss_pages = clamp_counter(count);

            bpf_core_read(&count, sizeof(count), &mm_old->rss_stat.count[MM_SWAPENTS]);
            event->swap_pages = clamp_counter(count);

            event->rss_valid = true;
        } else if (bpf_core_field_exists(mm->rss_stat)) {
            // Kernel >= 6.2: percpu_counter array. Reading .count gives the
            // batched total and omits the unflushed per-CPU deltas, so the
            // figure is approximate by up to a few pages per CPU.
            s64 count = 0;

            bpf_core_read(&count, sizeof(count), &mm->rss_stat[MM_FILEPAGES].count);
            event->file_rss_pages = clamp_counter(count);

            bpf_core_read(&count, sizeof(count), &mm->rss_stat[MM_ANONPAGES].count);
            event->anon_rss_pages = clamp_counter(count);

            bpf_core_read(&count, sizeof(count), &mm->rss_stat[MM_SHMEMPAGES].count);
            event->shmem_rss_pages = clamp_counter(count);

            bpf_core_read(&count, sizeof(count), &mm->rss_stat[MM_SWAPENTS].count);
            event->swap_pages = clamp_counter(count);

            event->rss_valid = true;
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
