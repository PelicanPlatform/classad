;; aggsum -- a sum that keeps its integer and real contributions apart, and takes
;; column values.
;;
;; It is the worked example of the aggregate ABI: four operations over a state the
;; host owns, which here is 24 bytes.
;;
;;   0..7    i64  the sum of the integer and boolean values
;;   8..15   f64  the sum of the real values
;;   16..19  i32  nonzero once a real has been seen
;;   20..23  i32  how many values were folded
;;
;; THREE INDEPENDENT FIELDS ON PURPOSE. A merge that forgot one of them -- or a
;; host that handed merge the same state twice -- gives the right answer for a
;; fixture whose values are all of one kind, which is exactly the fixture anyone
;; writes first. Splitting the integer and real sums means a merge has to carry
;; both, and the count means it has to carry something no arithmetic on the sums
;; could reconstruct.
;;
;; Summing integers in i64 rather than through the float keeps them EXACT under
;; any fold order, which matters because the engine fans the pass out across
;; segments and the order two partial states are combined in is a race. The real
;; sum is not exact under reordering, and cannot be: float addition is not
;; associative. That is a property of the arithmetic, not of this module.
;;
;; It exports the optional numeric fold, which is this ABI's way of declaring that
;; the aggregate may be fed from a column -- that its answer does not depend on
;; the order values arrive in, and that it does not count what it was never
;; offered. Both hold: addition commutes, and a value the column pass withholds is
;; a value this module would have skipped anyway. The declaration is the same
;; function under a second name, which is the common case.
(module
  (memory (export "memory") 1)

  (func (export "classad_udaf_abi_version") (result i32) (i32.const 1))

  (func $init (param $out i32) (param $cap i32) (result i32)
    (if (i32.lt_u (local.get $cap) (i32.const 24))
      (then (return (i32.const -1))))
    (memory.fill (local.get $out) (i32.const 0) (i32.const 24))
    (i32.const 24))

  (func $fold (param $sp i32) (param $sl i32) (param $vp i32) (param $n i32)
              (param $op i32) (param $oc i32) (result i32)
    (local $i i32) (local $slot i32) (local $tag i32)
    (local $isum i64) (local $rsum f64) (local $saw i32) (local $cnt i32)
    (if (i32.ne (local.get $sl) (i32.const 24))
      (then (return (i32.const -1))))
    (if (i32.lt_u (local.get $oc) (i32.const 24))
      (then (return (i32.const -1))))
    (local.set $isum (i64.load (local.get $sp)))
    (local.set $rsum (f64.load offset=8 (local.get $sp)))
    (local.set $saw (i32.load offset=16 (local.get $sp)))
    (local.set $cnt (i32.load offset=20 (local.get $sp)))
    (block $done
      (loop $next
        (br_if $done (i32.ge_u (local.get $i) (local.get $n)))
        (local.set $slot
          (i32.add (local.get $vp) (i32.mul (local.get $i) (i32.const 16))))
        (local.set $tag (i32.load (local.get $slot)))
        ;; integer (3) and boolean (2) both go to the exact integer sum.
        (if (i32.or (i32.eq (local.get $tag) (i32.const 3))
                    (i32.eq (local.get $tag) (i32.const 2)))
          (then
            (local.set $isum
              (i64.add (local.get $isum) (i64.load offset=8 (local.get $slot))))
            (local.set $cnt (i32.add (local.get $cnt) (i32.const 1))))
          (else
            (if (i32.eq (local.get $tag) (i32.const 4))
              (then
                (local.set $rsum
                  (f64.add (local.get $rsum) (f64.load offset=8 (local.get $slot))))
                (local.set $saw (i32.const 1))
                (local.set $cnt (i32.add (local.get $cnt) (i32.const 1)))))))
        ;; Everything else -- undefined, error, a string -- is skipped, which is
        ;; what the built-in sum does and what lets this module promise the
        ;; column pass that withholding them changes nothing.
        (local.set $i (i32.add (local.get $i) (i32.const 1)))
        (br $next)))
    (i64.store (local.get $op) (local.get $isum))
    (f64.store offset=8 (local.get $op) (local.get $rsum))
    (i32.store offset=16 (local.get $op) (local.get $saw))
    (i32.store offset=20 (local.get $op) (local.get $cnt))
    (i32.const 24))

  (func $merge (param $ap i32) (param $al i32) (param $bp i32) (param $bl i32)
               (param $op i32) (param $oc i32) (result i32)
    (if (i32.or (i32.ne (local.get $al) (i32.const 24))
                (i32.ne (local.get $bl) (i32.const 24)))
      (then (return (i32.const -1))))
    (if (i32.lt_u (local.get $oc) (i32.const 24))
      (then (return (i32.const -1))))
    (i64.store (local.get $op)
      (i64.add (i64.load (local.get $ap)) (i64.load (local.get $bp))))
    (f64.store offset=8 (local.get $op)
      (f64.add (f64.load offset=8 (local.get $ap)) (f64.load offset=8 (local.get $bp))))
    (i32.store offset=16 (local.get $op)
      (i32.or (i32.load offset=16 (local.get $ap)) (i32.load offset=16 (local.get $bp))))
    (i32.store offset=20 (local.get $op)
      (i32.add (i32.load offset=20 (local.get $ap)) (i32.load offset=20 (local.get $bp))))
    (i32.const 24))

  ;; An integer answer while every value was an integer, a real once one was not:
  ;; the same promotion the reference's own sum does, so the two can be compared
  ;; as text rather than as numbers.
  (func $result (param $sp i32) (param $sl i32) (param $rp i32) (result i32)
    (if (i32.ne (local.get $sl) (i32.const 24))
      (then (return (i32.const 1))))
    (i32.store offset=4 (local.get $rp) (i32.const 0))
    (if (i32.load offset=16 (local.get $sp))
      (then
        (i32.store (local.get $rp) (i32.const 4))
        (f64.store offset=8 (local.get $rp)
          (f64.add (f64.load offset=8 (local.get $sp))
                   (f64.convert_i64_s (i64.load (local.get $sp))))))
      (else
        (i32.store (local.get $rp) (i32.const 3))
        (i64.store offset=8 (local.get $rp) (i64.load (local.get $sp)))))
    (i32.const 0))

  (export "classad_udaf_init" (func $init))
  (export "classad_udaf_fold" (func $fold))
  (export "classad_udaf_fold_num" (func $fold))
  (export "classad_udaf_merge" (func $merge))
  (export "classad_udaf_result" (func $result))
)
