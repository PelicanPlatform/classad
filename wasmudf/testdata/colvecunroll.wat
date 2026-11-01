;; colvecunroll -- the same doubling as colvecsimd, with eight records per loop
;; back-edge written in ORDINARY i64 instructions rather than vector ones.
;;
;; It is the control. colvecsimd is faster than colvecdouble for two reasons at
;; once -- it uses vector lanes AND it goes round its loop an eighth as often --
;; and on an interruptible runtime the second is charged for and the first is not.
;; Running this against it holds the back-edge count equal, so what is left is
;; what the vector instructions themselves are worth.
(module
  (memory (export "memory") 1)

  (func (export "classad_udf_abi_version") (result i32) (i32.const 2))

  (func $declineAll (param $decl i32) (param $n i32)
    (local $w i32) (local $words i32)
    (local.set $words (i32.shr_u (i32.add (local.get $n) (i32.const 63)) (i32.const 6)))
    (block $end (loop $next
      (br_if $end (i32.ge_u (local.get $w) (local.get $words)))
      (i64.store (i32.add (local.get $decl) (i32.shl (local.get $w) (i32.const 3)))
                 (i64.const -1))
      (local.set $w (i32.add (local.get $w) (i32.const 1)))
      (br $next))))

  (func (export "classad_udf_call_vec")
        (param $n i32) (param $args i32) (param $argc i32)
        (param $res i32) (param $decl i32) (result i32)
    (local $iPtr i32) (local $rI i32)
    (local $i i32) (local $src i32) (local $dst i32)

    (if (i32.ne (local.get $argc) (i32.const 1))
      (then (return (i32.const 1))))
    (if (i32.ne (i32.load (local.get $args)) (i32.const 2))
      (then (return (i32.const 1))))
    (if (i32.ne (i32.load offset=4 (local.get $args)) (i32.const 0))
      (then (return (i32.const 1))))
    (if (i32.ne (i32.load8_u (i32.load offset=8 (local.get $args))) (i32.const 0))
      (then
        (call $declineAll (local.get $decl) (local.get $n))
        (return (i32.const 0))))

    (local.set $iPtr (i32.load offset=12 (local.get $args)))
    (local.set $rI (i32.load offset=12 (local.get $res)))
    (i32.store8 (i32.load offset=8 (local.get $res)) (i32.const 0))

    (block $end (loop $next
      (br_if $end (i32.gt_u (i32.add (local.get $i) (i32.const 8)) (local.get $n)))
      (local.set $src (i32.add (local.get $iPtr) (i32.shl (local.get $i) (i32.const 3))))
      (local.set $dst (i32.add (local.get $rI) (i32.shl (local.get $i) (i32.const 3))))
      (i64.store offset=0 (local.get $dst)
        (i64.mul (i64.load offset=0 (local.get $src)) (i64.const 2)))
      (i64.store offset=8 (local.get $dst)
        (i64.mul (i64.load offset=8 (local.get $src)) (i64.const 2)))
      (i64.store offset=16 (local.get $dst)
        (i64.mul (i64.load offset=16 (local.get $src)) (i64.const 2)))
      (i64.store offset=24 (local.get $dst)
        (i64.mul (i64.load offset=24 (local.get $src)) (i64.const 2)))
      (i64.store offset=32 (local.get $dst)
        (i64.mul (i64.load offset=32 (local.get $src)) (i64.const 2)))
      (i64.store offset=40 (local.get $dst)
        (i64.mul (i64.load offset=40 (local.get $src)) (i64.const 2)))
      (i64.store offset=48 (local.get $dst)
        (i64.mul (i64.load offset=48 (local.get $src)) (i64.const 2)))
      (i64.store offset=56 (local.get $dst)
        (i64.mul (i64.load offset=56 (local.get $src)) (i64.const 2)))
      (local.set $i (i32.add (local.get $i) (i32.const 8)))
      (br $next)))

    ;; The tail, a record at a time. A batch width is not a multiple of eight
    ;; because the engine said so; it is whatever the block had left.
    (block $end2 (loop $next2
      (br_if $end2 (i32.ge_u (local.get $i) (local.get $n)))
      (local.set $src (i32.shl (local.get $i) (i32.const 3)))
      (i64.store (i32.add (local.get $rI) (local.get $src))
        (i64.mul (i64.load (i32.add (local.get $iPtr) (local.get $src))) (i64.const 2)))
      (local.set $i (i32.add (local.get $i) (i32.const 1)))
      (br $next2)))
    (i32.const 0))

  (func (export "classad_udf_call")
        (param $a i32) (param $argc i32) (param $res i32) (result i32)
    (local $tag i32)
    (if (i32.ne (local.get $argc) (i32.const 1))
      (then (return (i32.const 1))))
    (local.set $tag (i32.load (local.get $a)))
    (i32.store (local.get $res) (local.get $tag))
    (i32.store offset=4 (local.get $res) (i32.const 0))
    (i64.store offset=8 (local.get $res) (i64.const 0))
    (block $done
      (if (i32.eq (local.get $tag) (i32.const 3))
        (then
          (i64.store offset=8 (local.get $res)
            (i64.mul (i64.load offset=8 (local.get $a)) (i64.const 2)))
          (br $done)))
      (if (i32.eq (local.get $tag) (i32.const 4))
        (then
          (i64.store offset=8 (local.get $res)
            (i64.reinterpret_f64
              (f64.mul (f64.reinterpret_i64 (i64.load offset=8 (local.get $a)))
                       (f64.const 2))))
          (br $done)))
      (if (i32.le_u (local.get $tag) (i32.const 1))
        (then (br $done)))
      (return (i32.const 2)))
    (i32.const 0))
)
